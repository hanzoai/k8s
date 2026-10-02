package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// kube is the cluster API as this pod's service account, which may get and patch
// nodes and nothing else (infra/aws/k8s/disk.yaml).
type kube struct {
	base string
	c    *http.Client
}

const account = "/var/run/secrets/kubernetes.io/serviceaccount/"

func inCluster() (*kube, error) {
	h, p := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if h == "" || p == "" {
		return nil, errors.New("not in a cluster: KUBERNETES_SERVICE_HOST is unset")
	}
	ca, err := os.ReadFile(account + "ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("service account ca.crt holds no certificate")
	}
	return &kube{
		base: "https://" + net.JoinHostPort(h, p),
		c: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}},
	}, nil
}

// nodeState is what this program reads of its node.
type nodeState struct {
	Pressure  bool // carries Label=Pressure
	Cordoned  bool // spec.unschedulable
	OurCordon bool // the cordon is this program's (annotation Cordoned)
}

func (k *kube) node(ctx context.Context, name string) (nodeState, error) {
	var n struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
	}
	if err := k.do(ctx, http.MethodGet, "/api/v1/nodes/"+name, nil, &n); err != nil {
		return nodeState{}, err
	}
	return nodeState{
		Pressure:  n.Metadata.Labels[Label] == Pressure,
		Cordoned:  n.Spec.Unschedulable,
		OurCordon: n.Metadata.Annotations[Cordoned] == "true",
	}, nil
}

// patch applies a JSON merge patch to the node; a null value deletes a key.
func (k *kube) patch(ctx context.Context, name string, body map[string]any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return k.do(ctx, http.MethodPatch, "/api/v1/nodes/"+name, b, nil)
}

func (k *kube) do(ctx context.Context, method, path string, body []byte, out any) error {
	// Re-read every call: the projected token is rotated under a running pod.
	tok, err := os.ReadFile(account + "token")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/merge-patch+json")
	}
	resp, err := k.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
