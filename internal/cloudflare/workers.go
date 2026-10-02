package cloudflare

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"strings"
)

// Binding is a Worker's binding as the upload API takes it: a KV
// namespace, an email sender, a plain text variable.
type Binding map[string]string

// FindOrCreateKVNamespace returns the ID of the account's KV namespace
// titled title, creating it if there's none.
func (c Client) FindOrCreateKVNamespace(accountID, title string) (string, error) {
	var list []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := c.call("GET", "/accounts/"+accountID+"/storage/kv/namespaces?per_page=100", nil, &list); err != nil {
		return "", err
	}
	for _, n := range list {
		if n.Title == title {
			return n.ID, nil
		}
	}
	var created struct {
		ID string `json:"id"`
	}
	err := c.call("POST", "/accounts/"+accountID+"/storage/kv/namespaces", map[string]string{"title": title}, &created)
	return created.ID, err
}

// DeleteKVNamespace removes a KV namespace with what it holds.
func (c Client) DeleteKVNamespace(accountID, id string) error {
	return c.call("DELETE", "/accounts/"+accountID+"/storage/kv/namespaces/"+url.PathEscape(id), nil, nil)
}

// UploadWorker creates or replaces the Worker name with one ES module.
func (c Client) UploadWorker(accountID, name, module, compatibilityDate string, bindings []Binding) error {
	meta, err := json.Marshal(map[string]any{
		"main_module":        "worker.js",
		"compatibility_date": compatibilityDate,
		"bindings":           bindings,
	})
	if err != nil {
		return err
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="metadata"`},
		"Content-Type":        {"application/json"},
	})
	if err != nil {
		return err
	}
	if _, err := part.Write(meta); err != nil {
		return err
	}
	part, err = w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="worker.js"; filename="worker.js"`},
		"Content-Type":        {"application/javascript+module"},
	})
	if err != nil {
		return err
	}
	if _, err := part.Write([]byte(module)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	path := "/accounts/" + accountID + "/workers/scripts/" + url.PathEscape(name)
	resp, err := c.do("PUT", path, w.FormDataContentType(), &body, int64(body.Len()))
	if err != nil {
		return err
	}
	return c.decode("PUT", path, resp, nil)
}

// SetWorkerCrons replaces the Worker's cron triggers.
func (c Client) SetWorkerCrons(accountID, name string, crons []string) error {
	var schedules []map[string]string
	for _, cron := range crons {
		schedules = append(schedules, map[string]string{"cron": cron})
	}
	return c.call("PUT", "/accounts/"+accountID+"/workers/scripts/"+url.PathEscape(name)+"/schedules", schedules, nil)
}

// DeleteWorker removes the Worker name; a no-op if there's none.
func (c Client) DeleteWorker(accountID, name string) error {
	err := c.call("DELETE", "/accounts/"+accountID+"/workers/scripts/"+url.PathEscape(name)+"?force=true", nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// isNotFound tells a refusal for something that isn't there, such as a
// Worker already deleted (workers.api.error.script_not_found).
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "not_found")
}

// WorkerExists reports whether the account has the Worker name.
func (c Client) WorkerExists(accountID, name string) (bool, error) {
	err := c.call("GET", "/accounts/"+accountID+"/workers/scripts/"+url.PathEscape(name)+"/settings", nil, nil)
	if isNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
