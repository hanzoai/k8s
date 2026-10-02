package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ebs grows the node's own EBS volumes with the node's instance role, whose
// policy (infra/aws/main.tf, "disk") allows ec2:ModifyVolume only on volumes
// tagged hanzo.ai/disk=grow. Credentials come from the instance metadata
// service, which answers only on the node's own network (hop limit 1), which is
// why this pod runs on the host network.
type ebs struct {
	c      *http.Client
	region string
}

const imds = "http://169.254.169.254"

func (e *ebs) meta(ctx context.Context, path string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, imds+"/latest/api/token", nil)
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "300")
	resp, err := e.c.Do(req)
	if err != nil {
		return "", err
	}
	tok, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("imds token: %s", resp.Status)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, imds+path, nil)
	req.Header.Set("X-aws-ec2-metadata-token", string(tok))
	resp, err = e.c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("imds %s: %s", path, resp.Status)
	}
	return strings.TrimSpace(string(b)), nil
}

type creds struct {
	AccessKeyId, SecretAccessKey, Token string
}

func (e *ebs) creds(ctx context.Context) (creds, error) {
	role, err := e.meta(ctx, "/latest/meta-data/iam/security-credentials/")
	if err != nil {
		return creds{}, err
	}
	raw, err := e.meta(ctx, "/latest/meta-data/iam/security-credentials/"+strings.Fields(role)[0])
	if err != nil {
		return creds{}, err
	}
	var c creds
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return creds{}, err
	}
	return c, nil
}

// call makes one EC2 Query API call, signed with Signature Version 4.
func (e *ebs) call(ctx context.Context, params url.Values, out any) error {
	c, err := e.creds(ctx)
	if err != nil {
		return err
	}
	params.Set("Version", "2016-11-15")
	body := params.Encode()
	hostname := "ec2." + e.region + ".amazonaws.com"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+hostname+"/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	sign(req, body, hostname, e.region, "ec2", c, time.Now())
	resp, err := e.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var fault struct {
			Errors []struct {
				Code    string `xml:"Code"`
				Message string `xml:"Message"`
			} `xml:"Errors>Error"`
		}
		if xml.Unmarshal(data, &fault) == nil && len(fault.Errors) > 0 {
			return &awsError{Code: fault.Errors[0].Code, Message: fault.Errors[0].Message}
		}
		return fmt.Errorf("%s: %s", params.Get("Action"), resp.Status)
	}
	return xml.Unmarshal(data, out)
}

type awsError struct{ Code, Message string }

func (e *awsError) Error() string { return e.Code + ": " + e.Message }

// sign adds a Signature Version 4 Authorization header to req.
func sign(req *http.Request, body, hostname, region, service string, c creds, now time.Time) {
	stamp := now.UTC().Format("20060102T150405Z")
	date := stamp[:8]
	req.Header.Set("X-Amz-Date", stamp)
	headers := []string{"content-type", "host", "x-amz-date"}
	values := map[string]string{"content-type": req.Header.Get("Content-Type"), "host": hostname, "x-amz-date": stamp}
	if c.Token != "" {
		req.Header.Set("X-Amz-Security-Token", c.Token)
		headers = append(headers, "x-amz-security-token")
		values["x-amz-security-token"] = c.Token
	}
	var canon strings.Builder
	for _, h := range headers {
		canon.WriteString(h + ":" + values[h] + "\n")
	}
	signed := strings.Join(headers, ";")
	request := strings.Join([]string{req.Method, "/", "", canon.String(), signed, hexSum(body)}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", stamp, scope, hexSum(request)}, "\n")
	sig := hex.EncodeToString(mac(key(c.SecretAccessKey, date, region, service), toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKeyId+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func key(secret, date, region, service string) []byte {
	k := mac([]byte("AWS4"+secret), date)
	k = mac(k, region)
	k = mac(k, service)
	return mac(k, "aws4_request")
}

func mac(k []byte, s string) []byte {
	h := hmac.New(sha256.New, k)
	h.Write([]byte(s))
	return h.Sum(nil)
}

func hexSum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// size is the volume's size in GiB.
func (e *ebs) size(ctx context.Context, vol string) (int, error) {
	var out struct {
		Volumes []struct {
			Size string `xml:"size"`
		} `xml:"volumeSet>item"`
	}
	if err := e.call(ctx, url.Values{"Action": {"DescribeVolumes"}, "VolumeId.1": {vol}}, &out); err != nil {
		return 0, err
	}
	if len(out.Volumes) != 1 {
		return 0, fmt.Errorf("%s: described %d volumes", vol, len(out.Volumes))
	}
	return strconv.Atoi(out.Volumes[0].Size)
}

// changed is when the volume's latest modification started, and its state;
// zero for a volume never modified.
func (e *ebs) changed(ctx context.Context, vol string) (time.Time, string, error) {
	var out struct {
		Mods []struct {
			State string    `xml:"modificationState"`
			Start time.Time `xml:"startTime"`
		} `xml:"volumeModificationSet>item"`
	}
	err := e.call(ctx, url.Values{"Action": {"DescribeVolumesModifications"}, "VolumeId.1": {vol}}, &out)
	var ae *awsError
	if errors.As(err, &ae) && ae.Code == "InvalidVolumeModification.NotFound" {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", err
	}
	var t time.Time
	var state string
	for _, m := range out.Mods {
		if m.Start.After(t) {
			t, state = m.Start, m.State
		}
	}
	return t, state, nil
}

func (e *ebs) grow(ctx context.Context, vol string, size int) error {
	var out struct{}
	return e.call(ctx, url.Values{"Action": {"ModifyVolume"}, "VolumeId": {vol}, "Size": {strconv.Itoa(size)}}, &out)
}

// cooldown is how long AWS makes a volume wait between modifications.
const cooldown = 6 * time.Hour
