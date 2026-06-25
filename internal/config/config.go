package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/caarlos0/env/v11"
	"github.com/goccy/go-json"
)

var (
	// ErrMissingIMAPHost is returned when IMAP host is not configured
	ErrMissingIMAPHost = errors.New("IMAP_HOST is required: set via environment variable or config file")
	// ErrMissingIMAPUsername is returned when IMAP username is not configured
	ErrMissingIMAPUsername = errors.New("IMAP_USERNAME is required: set via environment variable or config file")
	// ErrMissingIMAPPassword is returned when IMAP password is not configured
	ErrMissingIMAPPassword = errors.New("IMAP_PASSWORD is required: set via environment variable or config file")
)

// MSGraphConfig configures Microsoft Graph API access for Microsoft 365 mailboxes.
//
// This is used instead of IMAP when your M365 tenant requires OAuth authentication.
// Set "enabled": true and provide the three Azure credentials; IMAP config is then ignored.
//
// Azure app registration steps:
//  1. Register an app in Entra ID (https://entra.microsoft.com) and create a client secret.
//  2. Grant the application permission Mail.ReadWrite (not delegated) under Microsoft Graph.
//  3. Restrict the app to a single mailbox using Exchange PowerShell (strongly recommended):
//       New-ApplicationAccessPolicy \
//         -AccessRight RestrictAccess \
//         -AppId "<client_id>" \
//         -PolicyScopeGroupId "<mailbox_email>" \
//         -Description "Restrict parse-dmarc to DMARC reports mailbox"
//  4. Fill in tenant_id, client_id, client_secret, and mailbox below.
type MSGraphConfig struct {
	// Enabled switches the fetch backend to Microsoft Graph. IMAP config is ignored when true.
	Enabled bool `json:"enabled" env:"MSGRAPH_ENABLED" envDefault:"false"`
	// TenantID is the Azure AD / Entra ID tenant (directory) ID.
	TenantID string `json:"tenant_id" env:"MSGRAPH_TENANT_ID"`
	// ClientID is the application (client) ID from the Azure app registration.
	ClientID string `json:"client_id" env:"MSGRAPH_CLIENT_ID"`
	// ClientSecret is the client secret value from the Azure app registration.
	ClientSecret string `json:"client_secret" env:"MSGRAPH_CLIENT_SECRET"`
	// Mailbox is the email address of the mailbox to read (e.g. dmarc@example.com).
	Mailbox string `json:"mailbox" env:"MSGRAPH_MAILBOX"`
	// MailboxFolder is the folder to read from. Accepts well-known names (inbox, archive,
	// deleteditems, drafts, junkemail, outbox, sentitems) or a custom display name.
	MailboxFolder string `json:"mailbox_folder" env:"MSGRAPH_MAILBOX_FOLDER" envDefault:"inbox"`
	// MarkAsRead marks each processed message as read (default: true).
	MarkAsRead bool `json:"mark_as_read" env:"MSGRAPH_MARK_AS_READ" envDefault:"true"`
	// ProcessedFolder, when set, moves processed messages into this folder (created if absent).
	ProcessedFolder string `json:"processed_folder" env:"MSGRAPH_PROCESSED_FOLDER"`
}

func (m *MSGraphConfig) validate() error {
	if m.TenantID == "" {
		return errors.New("MSGRAPH_TENANT_ID is required when MSGraph is enabled")
	}
	if m.ClientID == "" {
		return errors.New("MSGRAPH_CLIENT_ID is required when MSGraph is enabled")
	}
	if m.ClientSecret == "" {
		return errors.New("MSGRAPH_CLIENT_SECRET is required when MSGraph is enabled")
	}
	if m.Mailbox == "" {
		return errors.New("MSGRAPH_MAILBOX is required when MSGraph is enabled")
	}
	return nil
}

// Config holds the application configuration
type Config struct {
	LogLevel    string         `json:"log_level" env:"LOG_LEVEL" envDefault:"info"`
	ColoredLogs bool           `json:"colored_logs" env:"COLORED_LOGS" envDefault:"false"`
	IMAP        IMAPConfig     `json:"imap"`
	MSGraph     MSGraphConfig  `json:"msgraph"`
	Database    DatabaseConfig `json:"database"`
	Server      ServerConfig   `json:"server"`
}

// IMAPConfig holds IMAP server configuration
type IMAPConfig struct {
	Host     string `json:"host" env:"IMAP_HOST"`
	Port     int    `json:"port" env:"IMAP_PORT" envDefault:"993"`
	Username string `json:"username" env:"IMAP_USERNAME"`
	Password string `json:"password" env:"IMAP_PASSWORD"`
	Mailbox  string `json:"mailbox" env:"IMAP_MAILBOX" envDefault:"INBOX"`
	UseTLS   bool   `json:"use_tls" env:"IMAP_USE_TLS" envDefault:"true"`

	MarkAsSeen       bool   `json:"mark_as_seen" env:"IMAP_MARK_AS_SEEN" envDefault:"true"`
	ProcessedMailbox string `json:"processed_mailbox" env:"IMAP_PROCESSED_MAILBOX"`
}

// DatabaseConfig holds database configuration
type DatabaseConfig struct {
	Path string `json:"path" env:"DATABASE_PATH"`
}

// ServerConfig holds web server configuration
type ServerConfig struct {
	Port int    `json:"port" env:"SERVER_PORT" envDefault:"8080"`
	Host string `json:"host" env:"SERVER_HOST" envDefault:""`
}

func defaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("cannot determine home directory")
	}
	return filepath.Join(home, ".parse-dmarc/db.sqlite"), nil
}

func fallbackDBPath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", errors.New("cannot determine home directory or current working directory")
	}
	return filepath.Join(cwd, ".parse-dmarc/db.sqlite"), nil
}

func ensureDBPathExists(dbPath string) error {
	parent := filepath.Dir(dbPath)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return errors.New("failed to create database directory at " + parent + ": " + err.Error() + " - ensure the path is writable or set DATABASE_PATH environment variable")
	}
	return nil
}

// Load loads configuration from a JSON file
func Load(path string) (*Config, error) {
	var cfg Config
	var err error

	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("parse env config: %w", err)
	}

	if _, err := os.Stat(path); err == nil {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config file %s: %w", path, err)
		}

		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse config file %s: %w", path, err)
		}
	}

	if cfg.IMAP.Port == 0 {
		cfg.IMAP.Port = 993
	}
	if cfg.IMAP.Mailbox == "" {
		cfg.IMAP.Mailbox = "INBOX"
	}
	if cfg.MSGraph.MailboxFolder == "" {
		cfg.MSGraph.MailboxFolder = "inbox"
	}
	if cfg.Database.Path == "" {
		cfg.Database.Path, err = defaultDBPath()
		if err != nil || ensureDBPathExists(cfg.Database.Path) != nil {
			cfg.Database.Path, err = fallbackDBPath()
			if err != nil {
				return nil, fmt.Errorf("resolve database path: %w", err)
			}
			err = ensureDBPathExists(cfg.Database.Path)
			if err != nil {
				return nil, fmt.Errorf("ensure database path: %w", err)
			}
		}
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}

	return &cfg, nil
}

// Validate checks that all required configuration values are set for the active fetch backend.
// Returns nil if valid, or an error describing the missing configuration.
func (c *Config) Validate() error {
	if c.MSGraph.Enabled {
		return c.MSGraph.validate()
	}
	if c.IMAP.Host == "" {
		return ErrMissingIMAPHost
	}
	if c.IMAP.Username == "" {
		return ErrMissingIMAPUsername
	}
	if c.IMAP.Password == "" {
		return ErrMissingIMAPPassword
	}
	return nil
}

// GenerateSample creates a sample configuration file
func GenerateSample(path string) error {
	dbPath, err := defaultDBPath()
	if err != nil {
		return fmt.Errorf("resolve default database path: %w", err)
	}
	sample := Config{
		LogLevel: "info",
		IMAP: IMAPConfig{
			Host:             "imap.example.com",
			Port:             993,
			Username:         "your-email@example.com",
			Password:         "your-password",
			Mailbox:          "INBOX",
			UseTLS:           true,
			MarkAsSeen:       true,
			ProcessedMailbox: "",
		},
		MSGraph: MSGraphConfig{
			Enabled:         false,
			TenantID:        "",
			ClientID:        "",
			ClientSecret:    "",
			Mailbox:         "dmarc@example.com",
			MailboxFolder:   "inbox",
			MarkAsRead:      true,
			ProcessedFolder: "",
		},
		Database: DatabaseConfig{
			Path: dbPath,
		},
		Server: ServerConfig{
			Port: 8080,
			Host: "0.0.0.0",
		},
	}

	data, err := json.MarshalIndent(sample, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sample config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write config file %s: %w", path, err)
	}

	return nil
}
