package cloudflare

import (
	"encoding/json"
	"net/url"
)

// FindOrCreateD1 returns the ID of the account's D1 database named name,
// creating it if there's none.
func (c Client) FindOrCreateD1(accountID, name string) (string, error) {
	var list []struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	}
	if err := c.call("GET", "/accounts/"+accountID+"/d1/database?name="+url.QueryEscape(name), nil, &list); err != nil {
		return "", err
	}
	for _, d := range list {
		if d.Name == name {
			return d.UUID, nil
		}
	}
	var created struct {
		UUID string `json:"uuid"`
	}
	err := c.call("POST", "/accounts/"+accountID+"/d1/database", map[string]string{"name": name}, &created)
	return created.UUID, err
}

// DeleteD1 removes a D1 database with its data; a no-op if it's gone.
func (c Client) DeleteD1(accountID, id string) error {
	err := c.call("DELETE", "/accounts/"+accountID+"/d1/database/"+url.PathEscape(id), nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// D1Query is one statement of a QueryD1 batch.
type D1Query struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty"`
}

// QueryD1 runs the statements in a database, in one batch, and returns the
// rows of each.
func (c Client) QueryD1(accountID, id string, queries ...D1Query) ([][]map[string]json.RawMessage, error) {
	var res []struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	if err := c.call("POST", "/accounts/"+accountID+"/d1/database/"+url.PathEscape(id)+"/query", map[string]any{"batch": queries}, &res); err != nil {
		return nil, err
	}
	out := make([][]map[string]json.RawMessage, len(res))
	for i, r := range res {
		out[i] = r.Results
	}
	return out, nil
}
