package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// StatusError is a non-2xx answer. It deliberately carries no body.
type StatusError struct{ Code int }

func (e *StatusError) Error() string   { return fmt.Sprintf("HTTP %d", e.Code) }
func (e *StatusError) StatusCode() int { return e.Code }

// BeaconClient talks to the SoroBeacon public API with a scoped token. The
// token needs only channel read and write on one workspace; see the chart
// README for the scopes.
type BeaconClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func (c *BeaconClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rdr)
	if err != nil {
		return fmt.Errorf("request could not be built")
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		// url.Error would quote the URL; keep only the fact.
		return fmt.Errorf("API unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &StatusError{Code: res.StatusCode}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out)
	}
	return nil
}

func (c *BeaconClient) List(ctx context.Context) ([]RemoteChannel, error) {
	var all []RemoteChannel
	cursor := ""
	for page := 0; page < 1000; page++ {
		var out struct {
			Channels   []RemoteChannel `json:"channels"`
			NextCursor string          `json:"next_cursor"`
		}
		path := "/api/v1/channels?limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Channels...)
		if out.NextCursor == "" {
			return all, nil
		}
		cursor = out.NextCursor
	}
	return all, nil
}

func (c *BeaconClient) Create(ctx context.Context, b ChannelBody) (int64, error) {
	var out RemoteChannel
	if err := c.do(ctx, http.MethodPost, "/api/v1/channels", b, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

func (c *BeaconClient) Update(ctx context.Context, id int64, b ChannelBody) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", id), b, nil)
}

func (c *BeaconClient) Delete(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/api/v1/channels/%d", id), nil, nil)
}

// KubeClient is a minimal in-cluster Kubernetes REST client. It is polling
// based and has no dependencies; see docs/operator.md for why.
type KubeClient struct {
	Host  string
	Token string
	HTTP  *http.Client
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount/"

// NewInClusterKube builds a client from the pod's service account.
func NewInClusterKube() (*KubeClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	tok, err := os.ReadFile(saDir + "token")
	if host == "" || err != nil {
		return nil, fmt.Errorf("not running in a cluster")
	}
	ca, err := os.ReadFile(saDir + "ca.crt")
	if err != nil {
		return nil, fmt.Errorf("cluster CA unreadable")
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &KubeClient{
		Host: "https://" + host + ":" + port, Token: strings.TrimSpace(string(tok)),
		HTTP: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
	}, nil
}

func (k *KubeClient) do(ctx context.Context, method, path, contentType string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.Host+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := k.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("kubernetes API unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &StatusError{Code: res.StatusCode}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(out)
	}
	return nil
}

const crBase = "/apis/sorobeacon.io/v1alpha1"

func (k *KubeClient) ListChannels(ctx context.Context) ([]Channel, error) {
	var out struct {
		Items []Channel `json:"items"`
	}
	if err := k.do(ctx, http.MethodGet, crBase+"/channels", "", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (k *KubeClient) GetSecret(ctx context.Context, ns, name string) (map[string][]byte, error) {
	var out struct {
		Data map[string]string `json:"data"`
	}
	if err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(ns)+"/secrets/"+url.PathEscape(name), "", nil, &out); err != nil {
		return nil, err
	}
	data := map[string][]byte{}
	for key, v := range out.Data {
		raw, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("secret data is not base64")
		}
		data[key] = raw
	}
	return data, nil
}

func (k *KubeClient) SetFinalizers(ctx context.Context, ns, name string, finalizers []string) error {
	if finalizers == nil {
		finalizers = []string{}
	}
	return k.do(ctx, http.MethodPatch, crBase+"/namespaces/"+url.PathEscape(ns)+"/channels/"+url.PathEscape(name),
		"application/merge-patch+json", map[string]any{"metadata": map[string]any{"finalizers": finalizers}}, nil)
}

func (k *KubeClient) UpdateStatus(ctx context.Context, ns, name string, st ChannelStatus) error {
	return k.do(ctx, http.MethodPatch, crBase+"/namespaces/"+url.PathEscape(ns)+"/channels/"+url.PathEscape(name)+"/status",
		"application/merge-patch+json", map[string]any{"status": st}, nil)
}
