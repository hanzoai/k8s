package registry

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// AllowPrivateHostsEnv, when set, disables the non-routable-address rejection on
// an apiserver host. Off in production; set ONLY by a test whose apiserver runs on
// loopback.
const AllowPrivateHostsEnv = "K8S_ALLOW_PRIVATE_HOSTS"

// SafeRESTConfig parses a kubeconfig into a REST config, and is the ONLY place in
// this program that parses one. After the consolidation that created this repo,
// nobody else in the fleet holds a kubeconfig at all — which is what makes "one
// gate" a structural fact rather than a convention every new caller must remember.
//
// It REFUSES:
//
//   - CREDENTIAL PLUGINS. rest.Config.ExecProvider runs a local binary with this
//     process's full environment inherited, so a kubeconfig that carries one is
//     remote code execution by whoever pasted it. AuthProvider likewise shells out.
//   - SSRF TARGETS. The apiserver host is the ACTUAL dial target, so it is what
//     gets guarded: https only, and never loopback, private, link-local, the
//     instance-metadata address, unspecified or multicast. Guarding an endpoint
//     that is not the one dialed is the shape of guard that does nothing.
//
// A pure DNS rebind after this check remains possible — client-go re-resolves at
// dial — and is the same accepted caveat every outbound-fetch guard in the fleet
// carries. It is stated rather than papered over.
func SafeRESTConfig(kubeconfig []byte) (*rest.Config, error) {
	if len(kubeconfig) == 0 {
		return nil, fmt.Errorf("kubeconfig is empty")
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig unusable: %w", err)
	}
	if cfg.ExecProvider != nil {
		return nil, fmt.Errorf("kubeconfig uses an exec credential plugin, which is not permitted")
	}
	if cfg.AuthProvider != nil {
		return nil, fmt.Errorf("kubeconfig uses an auth-provider plugin, which is not permitted")
	}
	if err := guardHost(cfg.Host); err != nil {
		return nil, err
	}
	return cfg, nil
}

func guardHost(rawHost string) error {
	u, err := url.Parse(strings.TrimSpace(rawHost))
	if err != nil || u.Host == "" {
		return fmt.Errorf("cluster apiserver endpoint is not a valid URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("cluster apiserver endpoint must be https")
	}
	if os.Getenv(AllowPrivateHostsEnv) != "" {
		return nil
	}
	host := u.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") {
		return fmt.Errorf("cluster apiserver host is not permitted")
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		r, rerr := net.LookupIP(host)
		if rerr != nil || len(r) == 0 {
			return fmt.Errorf("cluster apiserver host does not resolve")
		}
		ips = r
	}
	for _, ip := range ips {
		if blocked(ip) {
			return fmt.Errorf("cluster apiserver resolves to a non-routable address")
		}
	}
	return nil
}

func blocked(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}
