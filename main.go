// Command k8s is Hanzo's Kubernetes service: the ONE place in the fleet that holds a
// cluster credential and the ONE API — /v1/k8s — through which anything reaches a
// cluster.
//
// It builds as its own binary. A host composes it at run time; run directly it is an
// ordinary HIP-0119 service on its own port. Same binary, no second code path.
//
// # Why this is a service and not a library
//
// A cluster client linked into N apps means N binaries holding cluster credentials.
// Before this repo existed, nine places in the fleet built one: two with the ambient
// in-cluster ServiceAccount, one with a cluster-admin kubeconfig minted on demand for
// every cluster on the house account, and one with a kubeconfig read out of a database
// row and fed to a function that applied arbitrary YAML. That last one is the shape of
// the whole problem: with it, no ClusterRole smaller than "everything" could be
// written, so nothing was least-privileged and no reviewer could bound a blast radius.
//
// One owner, named ops, credentials in KMS. Consumers ASK.
//
// # Multi-cluster from the first line
//
// lux runs lux-k8s. zoo runs zoo-k8s. Customers run their own. So an org's resources
// may live on a different cluster from Hanzo's, and "our cluster" is not a thing this
// program can mean. Every op names its cluster, resolved through the registry against
// the calling org — and there is no ambient client to fall back to, because
// registry.Reach is the only client constructor in the module and it takes a name.
package main

import (
	"fmt"
	"os"

	"github.com/zap-proto/zip"
	"github.com/zap-proto/zip/middleware"

	"github.com/hanzoai/k8s/ops"
	"github.com/hanzoai/k8s/registry"
)

// name is this app's ONE name: the binary, the socket stem, the Declaration name and
// the <org>-<app> IAM segment. No mapping table.
const name = "k8s"

// defaultAddr is where this serves when run DIRECTLY rather than by a host. Under a
// host, zip.Addr ignores it and uses the private socket the host created — which is the
// whole plugin side of the transport contract, and the reason a fixed bind here would
// make every child but the first die on "address already in use".
const defaultAddr = ":9663"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
}

func run() error {
	app := zip.New(zip.Config{AppName: name})
	app.Use(middleware.Recover(), middleware.RequestID(), middleware.Logger(app.Logger()))

	// The store is the KMS peer over the call plane. It is constructed, not dialed:
	// zip.Ask dials lazily on first use, so nothing here touches a peer before the
	// describe check below — which is required, because a projection is a function of
	// the code and a describe run must not need a running fleet.
	ops.Ops{Reg: registry.New(registry.Sealed{})}.Mount(app)

	if done, err := app.Described(); done {
		return err
	}
	return app.Listen(zip.Addr(defaultAddr))
}
