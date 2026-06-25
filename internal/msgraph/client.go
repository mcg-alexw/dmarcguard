package msgraph

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	gojson "github.com/goccy/go-json"
	"github.com/meysam81/parse-dmarc/internal/config"
	"github.com/rs/zerolog"
	"golang.org/x/oauth2/clientcredentials"
)

const graphBase = "https://graph.microsoft.com/v1.0"

// Client fetches DMARC emails from a Microsoft 365 mailbox via the Graph API
// using client credentials (application) authentication.
type Client struct {
	cfg  *config.MSGraphConfig
	log  *zerolog.Logger
	http *http.Client
}

// Attachment holds a decoded file attachment from an email.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// Message represents an email with its DMARC-relevant attachments already fetched.
type Message struct {
	ID          string
	Subject     string
	From        string
	Date        string
	Attachments []Attachment
}

func NewClient(cfg *config.MSGraphConfig, log *zerolog.Logger) *Client {
	return &Client{cfg: cfg, log: log}
}

// Connect initialises an OAuth2 HTTP client via the client credentials flow and
// verifies credentials by performing an eager token fetch.
func (c *Client) Connect(ctx context.Context) error {
	oauthCfg := &clientcredentials.Config{
		ClientID:     c.cfg.ClientID,
		ClientSecret: c.cfg.ClientSecret,
		TokenURL:     "https://login.microsoftonline.com/" + c.cfg.TenantID + "/oauth2/v2.0/token",
		Scopes:       []string{"https://graph.microsoft.com/.default"},
	}
	ts := oauthCfg.TokenSource(ctx)
	if _, err := ts.Token(); err != nil {
		return fmt.Errorf("msgraph auth: %w", err)
	}
	c.http = oauthCfg.Client(ctx)
	return nil
}

// FetchMessages returns unread messages that have DMARC-relevant attachments.
// Attachments are decoded and ready for parsing. Call MarkAsRead / MoveMessages
// after processing to post-process the messages.
func (c *Client) FetchMessages(ctx context.Context) ([]Message, error) {
	folder := c.cfg.MailboxFolder
	if folder == "" {
		folder = "inbox"
	}
	folderID, err := c.resolveFolderID(ctx, folder)
	if err != nil {
		return nil, fmt.Errorf("resolve folder %q: %w", folder, err)
	}

	listURL := fmt.Sprintf(
		"%s/users/%s/mailFolders/%s/messages?$filter=isRead%%20eq%%20false&$top=50&$select=id,subject,from,receivedDateTime,hasAttachments",
		graphBase,
		url.PathEscape(c.cfg.Mailbox),
		url.PathEscape(folderID),
	)

	var messages []Message
	for listURL != "" {
		var resp struct {
			NextLink string `json:"@odata.nextLink"`
			Value    []struct {
				ID               string `json:"id"`
				Subject          string `json:"subject"`
				ReceivedDateTime string `json:"receivedDateTime"`
				HasAttachments   bool   `json:"hasAttachments"`
				From             struct {
					EmailAddress struct{ Address string `json:"address"` } `json:"emailAddress"`
				} `json:"from"`
			} `json:"value"`
		}
		if err := c.graphDo(ctx, "GET", listURL, nil, &resp); err != nil {
			return nil, fmt.Errorf("list messages: %w", err)
		}
		for _, item := range resp.Value {
			if !item.HasAttachments {
				continue
			}
			attachments, err := c.fetchAttachments(ctx, item.ID)
			if err != nil {
				c.log.Warn().Err(err).Str("message_id", item.ID).Str("subject", item.Subject).Msg("failed to fetch attachments, skipping message")
				continue
			}
			if len(attachments) == 0 {
				continue
			}
			messages = append(messages, Message{
				ID:          item.ID,
				Subject:     item.Subject,
				From:        item.From.EmailAddress.Address,
				Date:        item.ReceivedDateTime,
				Attachments: attachments,
			})
		}
		listURL = resp.NextLink
	}
	return messages, nil
}

// MarkAsRead marks the given messages as read.
func (c *Client) MarkAsRead(ctx context.Context, ids []string) error {
	body, _ := gojson.Marshal(map[string]bool{"isRead": true})
	mailbox := url.PathEscape(c.cfg.Mailbox)
	for _, id := range ids {
		patchURL := fmt.Sprintf("%s/users/%s/messages/%s", graphBase, mailbox, id)
		if err := c.graphDo(ctx, "PATCH", patchURL, body, nil); err != nil {
			return fmt.Errorf("mark message %s as read: %w", id, err)
		}
	}
	return nil
}

// EnsureFolder returns the ID of the named folder, creating it if absent.
func (c *Client) EnsureFolder(ctx context.Context, name string) (string, error) {
	id, err := c.resolveFolderID(ctx, name)
	if err == nil {
		return id, nil
	}
	createURL := fmt.Sprintf("%s/users/%s/mailFolders", graphBase, url.PathEscape(c.cfg.Mailbox))
	body, _ := gojson.Marshal(map[string]string{"displayName": name})
	var created struct {
		ID string `json:"id"`
	}
	if err := c.graphDo(ctx, "POST", createURL, body, &created); err != nil {
		return "", fmt.Errorf("create folder %q: %w", name, err)
	}
	return created.ID, nil
}

// MoveMessages moves the given messages to the folder identified by destFolderID.
func (c *Client) MoveMessages(ctx context.Context, ids []string, destFolderID string) error {
	body, _ := gojson.Marshal(map[string]string{"destinationId": destFolderID})
	mailbox := url.PathEscape(c.cfg.Mailbox)
	for _, id := range ids {
		moveURL := fmt.Sprintf("%s/users/%s/messages/%s/move", graphBase, mailbox, id)
		if err := c.graphDo(ctx, "POST", moveURL, body, nil); err != nil {
			return fmt.Errorf("move message %s: %w", id, err)
		}
	}
	return nil
}

// resolveFolderID returns the folder ID for a well-known name (e.g. "inbox") or
// a folder display name. Returns an error when the folder is not found.
func (c *Client) resolveFolderID(ctx context.Context, name string) (string, error) {
	wellKnown := map[string]bool{
		"inbox": true, "sentitems": true, "deleteditems": true,
		"drafts": true, "archive": true, "junkemail": true, "outbox": true,
	}
	if wellKnown[strings.ToLower(name)] {
		return strings.ToLower(name), nil
	}
	params := url.Values{}
	params.Set("$filter", fmt.Sprintf("displayName eq '%s'", strings.ReplaceAll(name, "'", "''")))
	listURL := fmt.Sprintf("%s/users/%s/mailFolders?%s", graphBase, url.PathEscape(c.cfg.Mailbox), params.Encode())
	var resp struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	if err := c.graphDo(ctx, "GET", listURL, nil, &resp); err != nil {
		return "", fmt.Errorf("list folders: %w", err)
	}
	if len(resp.Value) == 0 {
		return "", fmt.Errorf("folder %q not found", name)
	}
	return resp.Value[0].ID, nil
}

// fetchAttachments downloads all non-inline DMARC file attachments for a message.
func (c *Client) fetchAttachments(ctx context.Context, msgID string) ([]Attachment, error) {
	// $select cannot include contentBytes here: it is a fileAttachment-specific property
	// and Graph API validates $select against the base attachment type, returning HTTP 400.
	// We list metadata first, then fetch each qualifying attachment individually.
	listURL := fmt.Sprintf(
		"%s/users/%s/messages/%s/attachments?$select=id,name,contentType,isInline,@odata.type",
		graphBase, url.PathEscape(c.cfg.Mailbox), msgID,
	)
	var listResp struct {
		Value []struct {
			ID          string `json:"id"`
			ODataType   string `json:"@odata.type"`
			Name        string `json:"name"`
			ContentType string `json:"contentType"`
			IsInline    bool   `json:"isInline"`
		} `json:"value"`
	}
	if err := c.graphDo(ctx, "GET", listURL, nil, &listResp); err != nil {
		return nil, err
	}
	var result []Attachment
	mailbox := url.PathEscape(c.cfg.Mailbox)
	for _, a := range listResp.Value {
		if a.IsInline || a.ODataType != "#microsoft.graph.fileAttachment" {
			continue
		}
		if !isDMARCAttachment(a.Name) {
			continue
		}
		var full struct {
			ContentBytes string `json:"contentBytes"`
		}
		attURL := fmt.Sprintf("%s/users/%s/messages/%s/attachments/%s", graphBase, mailbox, msgID, a.ID)
		if err := c.graphDo(ctx, "GET", attURL, nil, &full); err != nil {
			c.log.Warn().Err(err).Str("name", a.Name).Msg("failed to fetch attachment content, skipping")
			continue
		}
		data, err := base64.StdEncoding.DecodeString(full.ContentBytes)
		if err != nil {
			c.log.Warn().Err(err).Str("name", a.Name).Msg("failed to base64-decode attachment, skipping")
			continue
		}
		result = append(result, Attachment{
			Filename:    a.Name,
			ContentType: a.ContentType,
			Data:        data,
		})
	}
	return result, nil
}

// graphDo executes a Graph API request and, when out is non-nil, JSON-decodes the response body.
func (c *Client) graphDo(ctx context.Context, method, reqURL string, body []byte, out any) error {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, errBody)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return gojson.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func isDMARCAttachment(filename string) bool {
	lower := strings.ToLower(filename)
	return strings.HasSuffix(lower, ".xml") ||
		strings.HasSuffix(lower, ".xml.gz") ||
		strings.HasSuffix(lower, ".zip") ||
		strings.Contains(lower, "dmarc")
}
