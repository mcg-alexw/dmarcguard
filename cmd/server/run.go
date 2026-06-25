package server

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/meysam81/parse-dmarc/internal/api"
	"github.com/meysam81/parse-dmarc/internal/config"
	"github.com/meysam81/parse-dmarc/internal/imap"
	"github.com/meysam81/parse-dmarc/internal/logger"
	"github.com/meysam81/parse-dmarc/internal/metrics"
	"github.com/meysam81/parse-dmarc/internal/msgraph"
	"github.com/meysam81/parse-dmarc/internal/parser"
	"github.com/meysam81/parse-dmarc/internal/storage"
)

func run(ctx context.Context, version, commit, date string) error {
	if genConfig {
		if err := config.GenerateSample(configPath); err != nil {
			return fmt.Errorf("failed to generate config: %w", err)
		}
		log.Info().Str("path", configPath).Msg("sample configuration written")
		return nil
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Reinitialize logger with config-derived level
	log = logger.NewLogger(cfg.LogLevel, !cfg.ColoredLogs)

	// Validate required IMAP configuration when fetching is enabled
	// (not serve-only and not MCP mode)
	if !serveOnly && !mcpMode && mcpHTTPAddr == "" {
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("configuration error: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0o755); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}

	store, err := storage.NewStorage(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Handle MCP mode
	if mcpMode || mcpHTTPAddr != "" {
		return runMCPServer(ctx, store, version)
	}

	// Initialize metrics if enabled
	var m *metrics.Metrics
	if metricsEnabled {
		m = metrics.New(version, commit, date)
		log.Info().Msg("prometheus metrics enabled at /metrics")
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	server := api.NewServer(store, cfg.Server.Host, cfg.Server.Port, m, log)
	serverErrChan := make(chan error, 1)
	go func() {
		serverErrChan <- server.Start(ctx)
	}()

	// Refresh metrics on startup
	server.RefreshMetrics()

	if serveOnly {
		log.Info().Msg("running in serve-only mode")
		select {
		case <-ctx.Done():
			log.Info().Msg("shutting down")
		case err := <-serverErrChan:
			if err != nil {
				return fmt.Errorf("server error: %w", err)
			}
		}
		return nil
	}

	if fetchOnce {
		if err := fetchReports(ctx, cfg, store, m); err != nil {
			return fmt.Errorf("failed to fetch reports: %w", err)
		}
		server.RefreshMetrics()
		log.Info().Msg("fetch complete")
		return nil
	}

	log.Info().Int("interval_seconds", fetchInterval).Msg("starting continuous fetch mode")

	if err := fetchReports(ctx, cfg, store, m); err != nil {
		log.Error().Err(err).Msg("initial fetch failed")
	}
	server.RefreshMetrics()

	ticker := time.NewTicker(time.Duration(fetchInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := fetchReports(ctx, cfg, store, m); err != nil {
				log.Error().Err(err).Msg("fetch failed")
			}
			server.RefreshMetrics()
		case <-ctx.Done():
			log.Info().Msg("shutting down")
			return nil
		case err := <-serverErrChan:
			if err != nil {
				return fmt.Errorf("server error: %w", err)
			}
		}
	}
}

func fetchReports(ctx context.Context, cfg *config.Config, store *storage.Storage, m *metrics.Metrics) error {
	if cfg.MSGraph.Enabled {
		return fetchReportsMSGraph(ctx, cfg, store, m)
	}
	return fetchReportsIMAP(cfg, store, m)
}

func fetchReportsIMAP(cfg *config.Config, store *storage.Storage, m *metrics.Metrics) error {
	log.Info().Msg("fetching DMARC reports via IMAP")

	fetchStart := time.Now()
	if m != nil {
		m.FetchCyclesTotal.Inc()
	}

	connectStart := time.Now()
	client := imap.NewClient(&cfg.IMAP, log)
	if err := client.Connect(); err != nil {
		if m != nil {
			m.RecordIMAPConnection(false, time.Since(connectStart))
			m.FetchErrors.Inc()
		}
		return fmt.Errorf("connect to IMAP server: %w", err)
	}
	if m != nil {
		m.RecordIMAPConnection(true, time.Since(connectStart))
	}
	defer func() { _ = client.Disconnect() }()

	result, err := client.FetchDMARCReports()
	if err != nil {
		if m != nil {
			m.FetchErrors.Inc()
		}
		return fmt.Errorf("fetch DMARC reports: %w", err)
	}

	if m != nil {
		m.ReportsFetched.Add(float64(len(result.Reports)))
	}

	if len(result.Reports) == 0 {
		log.Info().Msg("no new reports found")
		if m != nil {
			m.RecordFetchDuration(time.Since(fetchStart))
			m.LastFetchTimestamp.SetToCurrentTime()
		}
		return nil
	}

	log.Info().Int("count", len(result.Reports)).Msg("processing reports")

	processed := 0
	for _, report := range result.Reports {
		for _, attachment := range report.Attachments {
			if m != nil {
				m.AttachmentsTotal.Inc()
			}
			feedback, err := parser.ParseReport(attachment.Data)
			if err != nil {
				log.Warn().Err(err).Str("filename", attachment.Filename).Msg("failed to parse report")
				if m != nil {
					m.ReportParseErrors.Inc()
				}
				continue
			}
			if m != nil {
				m.ReportsParsed.Inc()
			}
			if err := store.SaveReport(feedback); err != nil {
				log.Error().Err(err).Str("report_id", feedback.ReportMetadata.ReportID).Msg("failed to save report")
				if m != nil {
					m.ReportStoreErrors.Inc()
				}
				continue
			}
			if m != nil {
				m.ReportsStored.Inc()
			}
			log.Info().
				Str("report_id", feedback.ReportMetadata.ReportID).
				Str("org", feedback.ReportMetadata.OrgName).
				Str("domain", feedback.PolicyPublished.Domain).
				Int("messages", feedback.GetTotalMessages()).
				Msg("saved report")
			processed++
		}
	}

	if len(result.MessageIDs) > 0 {
		if cfg.IMAP.MarkAsSeen {
			if err := client.MarkAsSeen(result.MessageIDs); err != nil {
				log.Error().Err(err).Msg("failed to mark messages as seen")
			} else {
				log.Info().Int("count", len(result.MessageIDs)).Msg("marked messages as seen")
			}
		}
		if cfg.IMAP.ProcessedMailbox != "" {
			if err := client.MoveMessages(result.MessageIDs, cfg.IMAP.ProcessedMailbox); err != nil {
				log.Error().Err(err).Msg("failed to move messages to processed mailbox")
			} else {
				log.Info().Int("count", len(result.MessageIDs)).Str("mailbox", cfg.IMAP.ProcessedMailbox).Msg("moved messages to processed mailbox")
			}
		}
	}

	if m != nil {
		m.RecordFetchDuration(time.Since(fetchStart))
		m.LastFetchTimestamp.SetToCurrentTime()
	}

	log.Info().Int("count", processed).Msg("reports processed")
	return nil
}

func fetchReportsMSGraph(ctx context.Context, cfg *config.Config, store *storage.Storage, m *metrics.Metrics) error {
	log.Info().Msg("fetching DMARC reports via Microsoft Graph")

	fetchStart := time.Now()
	if m != nil {
		m.FetchCyclesTotal.Inc()
	}

	client := msgraph.NewClient(&cfg.MSGraph, log)
	if err := client.Connect(ctx); err != nil {
		if m != nil {
			m.FetchErrors.Inc()
		}
		return fmt.Errorf("connect to Microsoft Graph: %w", err)
	}

	// Resolve the processed folder ID once so we don't look it up per-message.
	var processedFolderID string
	if cfg.MSGraph.ProcessedFolder != "" {
		var err error
		processedFolderID, err = client.EnsureFolder(ctx, cfg.MSGraph.ProcessedFolder)
		if err != nil {
			return fmt.Errorf("ensure processed folder: %w", err)
		}
	}

	messages, err := client.FetchMessages(ctx)
	if err != nil {
		if m != nil {
			m.FetchErrors.Inc()
		}
		return fmt.Errorf("fetch messages: %w", err)
	}

	if m != nil {
		m.ReportsFetched.Add(float64(len(messages)))
	}

	if len(messages) == 0 {
		log.Info().Msg("no new messages found")
		if m != nil {
			m.RecordFetchDuration(time.Since(fetchStart))
			m.LastFetchTimestamp.SetToCurrentTime()
		}
		return nil
	}

	log.Info().Int("count", len(messages)).Msg("processing messages")

	var messageIDs []string
	processed := 0

	for _, msg := range messages {
		messageIDs = append(messageIDs, msg.ID)
		for _, att := range msg.Attachments {
			if m != nil {
				m.AttachmentsTotal.Inc()
			}
			feedback, err := parser.ParseReport(att.Data)
			if err != nil {
				log.Warn().Err(err).Str("filename", att.Filename).Msg("failed to parse report")
				if m != nil {
					m.ReportParseErrors.Inc()
				}
				continue
			}
			if m != nil {
				m.ReportsParsed.Inc()
			}
			if err := store.SaveReport(feedback); err != nil {
				log.Error().Err(err).Str("report_id", feedback.ReportMetadata.ReportID).Msg("failed to save report")
				if m != nil {
					m.ReportStoreErrors.Inc()
				}
				continue
			}
			if m != nil {
				m.ReportsStored.Inc()
			}
			log.Info().
				Str("report_id", feedback.ReportMetadata.ReportID).
				Str("org", feedback.ReportMetadata.OrgName).
				Str("domain", feedback.PolicyPublished.Domain).
				Int("messages", feedback.GetTotalMessages()).
				Msg("saved report")
			processed++
		}
	}

	if len(messageIDs) > 0 {
		if cfg.MSGraph.MarkAsRead {
			if err := client.MarkAsRead(ctx, messageIDs); err != nil {
				log.Error().Err(err).Msg("failed to mark messages as read")
			} else {
				log.Info().Int("count", len(messageIDs)).Msg("marked messages as read")
			}
		}
		if processedFolderID != "" {
			if err := client.MoveMessages(ctx, messageIDs, processedFolderID); err != nil {
				log.Error().Err(err).Msg("failed to move messages to processed folder")
			} else {
				log.Info().Int("count", len(messageIDs)).Str("folder", cfg.MSGraph.ProcessedFolder).Msg("moved messages to processed folder")
			}
		}
	}

	if m != nil {
		m.RecordFetchDuration(time.Since(fetchStart))
		m.LastFetchTimestamp.SetToCurrentTime()
	}

	log.Info().Int("count", processed).Msg("reports processed")
	return nil
}
