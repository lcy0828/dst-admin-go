package notifications

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

var variablePattern = regexp.MustCompile(`\{\{([^{}]+)\}\}`)
var headerPattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
var variables = []string{"id", "event", "title", "message", "status", "roomId", "targetId", "jobId", "timestamp", "component", "currentVersion", "latestVersion", "releaseURL", "publishedAt", "detectedAt", "statusPath"}

func validate(input *ChannelInput, secret credentials) error {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 || strings.ContainsAny(input.Name, "\r\n\x00") || len(input.Events) > len(EventKinds) {
		return fmt.Errorf("%w: name/events", ErrInvalid)
	}
	input.Events = slices.Clone(input.Events)
	slices.Sort(input.Events)
	input.Events = slices.Compact(input.Events)
	for _, kind := range input.Events {
		if !slices.Contains(EventKinds, kind) {
			return fmt.Errorf("%w: event", ErrInvalid)
		}
	}
	if input.Enabled && len(input.Events) == 0 {
		return fmt.Errorf("%w: events required", ErrInvalid)
	}
	if input.Type == "email" {
		if len(input.Recipients) < 1 || len(input.Recipients) > 20 {
			return fmt.Errorf("%w: recipients", ErrInvalid)
		}
		for index, value := range input.Recipients {
			address, err := mail.ParseAddress(strings.TrimSpace(value))
			if err != nil || strings.ContainsAny(value, "\r\n\x00") || len(value) > 254 {
				return fmt.Errorf("%w: recipient", ErrInvalid)
			}
			input.Recipients[index] = address.Address
		}
		return nil
	}
	if input.Type != "webhook" {
		return fmt.Errorf("%w: type", ErrInvalid)
	}
	u, err := url.Parse(secret.URL)
	if err != nil || len(secret.URL) > 4096 || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || strings.ContainsAny(secret.URL, "\r\n\x00") {
		return fmt.Errorf("%w: webhook URL", ErrInvalid)
	}
	if input.Method == "" {
		input.Method = http.MethodPost
	}
	if input.Method != http.MethodPost && input.Method != http.MethodPut {
		return fmt.Errorf("%w: method", ErrInvalid)
	}
	if input.Format == "" {
		input.Format = "json"
	}
	if input.Format != "json" && input.Format != "text" {
		return fmt.Errorf("%w: format", ErrInvalid)
	}
	if input.ResponseMode == "" {
		input.ResponseMode = "http"
	}
	if !slices.Contains([]string{"http", "wecom", "dingtalk", "feishu"}, input.ResponseMode) {
		return fmt.Errorf("%w: response mode", ErrInvalid)
	}
	if input.Template == "" {
		input.Template = DefaultTemplate
	}
	if len(input.Template) > 32<<10 || len(secret.Headers) > 20 {
		return fmt.Errorf("%w: template/headers too large", ErrInvalid)
	}
	seenHeaders := make(map[string]bool, len(secret.Headers))
	for name, value := range secret.Headers {
		lower := strings.ToLower(name)
		if seenHeaders[lower] {
			return fmt.Errorf("%w: duplicate header", ErrInvalid)
		}
		seenHeaders[lower] = true
		if !headerPattern.MatchString(name) || len(name) > 128 || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") || slices.Contains([]string{"host", "content-length", "connection", "transfer-encoding", "trailer", "upgrade", "te"}, strings.ToLower(name)) {
			return fmt.Errorf("%w: header", ErrInvalid)
		}
	}
	for _, match := range variablePattern.FindAllStringSubmatch(input.Template, -1) {
		if !slices.Contains(variables, match[1]) {
			return fmt.Errorf("%w: unknown template variable", ErrInvalid)
		}
	}
	_, err = render(Channel{Format: input.Format, Template: input.Template}, Event{})
	return err
}

// Substitute parsed JSON strings, never raw JSON text: quotes/newlines in an
// event cannot change the shape of a custom webhook request.
func render(channel Channel, event Event) ([]byte, error) {
	values := map[string]string{"id": event.ID, "event": event.Kind, "title": event.Title, "message": event.Message, "status": event.Status, "roomId": event.RoomID, "targetId": event.TargetID, "jobId": event.JobID, "timestamp": event.OccurredAt.UTC().Format("2006-01-02T15:04:05Z")}
	if event.Release != nil {
		r := event.Release
		values["component"] = r.Component
		values["currentVersion"] = r.CurrentVersion
		values["latestVersion"] = r.LatestVersion
		values["releaseURL"] = r.ReleaseURL
		values["statusPath"] = r.StatusPath
		values["publishedAt"] = r.PublishedAt.UTC().Format("2006-01-02T15:04:05Z")
		values["detectedAt"] = r.DetectedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	replace := func(value string) string {
		return variablePattern.ReplaceAllStringFunc(value, func(match string) string { return values[match[2:len(match)-2]] })
	}
	if channel.Format == "text" {
		return []byte(replace(channel.Template)), nil
	}
	var document any
	if err := json.Unmarshal([]byte(channel.Template), &document); err != nil {
		return nil, fmt.Errorf("%w: JSON template", ErrInvalid)
	}
	var walk func(any) any
	walk = func(value any) any {
		switch item := value.(type) {
		case string:
			return replace(item)
		case []any:
			for index := range item {
				item[index] = walk(item[index])
			}
		case map[string]any:
			for key, field := range item {
				item[key] = walk(field)
			}
		}
		return value
	}
	return json.Marshal(walk(document))
}
