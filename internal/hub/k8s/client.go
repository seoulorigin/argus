package k8s

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	saDir      = "/var/run/secrets/kubernetes.io/serviceaccount"
	pageSize   = "500"
	maxBodyMiB = 64
)

// Client is a minimal read-only Kubernetes API client that authenticates with
// the Pod's ServiceAccount token. It only issues GET requests for list endpoints.
type Client struct {
	base      string
	tokenFile string
	http      *http.Client
}

// NewInClusterClient builds a client from the standard in-cluster environment.
// K8S_API_URL, K8S_TOKEN_FILE and K8S_CA_FILE override it (for tests and out-of-cluster use).
// It returns nil when no Kubernetes API is configured.
func NewInClusterClient() (*Client, error) {
	base := os.Getenv("K8S_API_URL")
	if base == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, nil
		}
		base = "https://" + net.JoinHostPort(host, port)
	}
	tokenFile := envOr("K8S_TOKEN_FILE", saDir+"/token")
	caFile := envOr("K8S_CA_FILE", saDir+"/ca.crt")

	transport := &http.Transport{
		MaxIdleConns:        4,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	if strings.HasPrefix(base, "https://") {
		pool := x509.NewCertPool()
		ca, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read cluster CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("cluster CA %s has no certificates", caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{base: strings.TrimRight(base, "/"), tokenFile: tokenFile, http: &http.Client{Transport: transport, Timeout: 10 * time.Second}}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// list fetches every page of a collection and hands each item's raw JSON to fn.
func (c *Client) list(ctx context.Context, path string, fn func(raw json.RawMessage) error) error {
	cont := ""
	for {
		q := url.Values{"limit": {pageSize}}
		if cont != "" {
			q.Set("continue", cont)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		// The token is projected and rotates, so read it on every request.
		if tok, err := os.ReadFile(c.tokenFile); err == nil {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
		}
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyMiB<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: %s: %.200s", path, resp.Status, body)
		}
		var page struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		for _, raw := range page.Items {
			if err := fn(raw); err != nil {
				return err
			}
		}
		if page.Metadata.Continue == "" {
			return nil
		}
		cont = page.Metadata.Continue
	}
}
