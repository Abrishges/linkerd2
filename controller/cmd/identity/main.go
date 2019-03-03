package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	idctl "github.com/linkerd/linkerd2/controller/identity"
	"github.com/linkerd/linkerd2/controller/k8s"
	"github.com/linkerd/linkerd2/pkg/admin"
	"github.com/linkerd/linkerd2/pkg/flags"
	"github.com/linkerd/linkerd2/pkg/identity"
	"github.com/linkerd/linkerd2/pkg/tls"
)

// TODO watch trustAnchorsPath for changes
// TODO watch issuerPath for changes
func main() {
	addr := flag.String("addr", ":8083", "address to serve on")
	adminAddr := flag.String("admin-addr", ":9996", "address of HTTP admin server")
	kubeConfigPath := flag.String("kubeconfig", "", "path to kube config")
	controllerNS := flag.String("controller-namespace", "linkerd",
		"namespace in which Linkerd is installed")
	trustDomain := flag.String("trust-domain", "cluster.local", "trust domain for identities")
	trustAnchorsPath := flag.String("trust-anchors",
		"/var/run/linkerd/identity/trust-anchors/trust-anchors.pem",
		"path to file or directory containing trust anchors")
	issuerPath := flag.String("issuer",
		"/var/run/linkerd/identity/issuer",
		"path to directoring containing issuer credentials")
	issuanceLifetime := flag.Duration("issuance-lifetime", 24*time.Hour,
		"The amount of time for which a signed certificate is valid")
	clockSkewAllowance := flag.Duration("clock-skew-allowance", 0,
		"The amount of time to allow for clock skew between nodes")
	// TODO flag.String("audience", "linkerd.io/identity", "Token audience")
	flags.ConfigureAndParse()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	dom, err := idctl.NewTrustDomain(*controllerNS, *trustDomain)
	if err != nil {
		log.Fatalf("Invalid trust domain: %s", err.Error())
	}

	tab, err := ioutil.ReadFile(*trustAnchorsPath)
	if err != nil {
		log.Fatalf("Failed to read trust anchors from %s: %s", *trustAnchorsPath, err)
	}
	trustAnchors, err := tls.DecodePEMCertPool(string(tab))
	if err != nil {
		log.Fatalf("Failed to read trust anchors from %s: %s", *trustAnchorsPath, err)
	}

	creds, err := tls.ReadPEMCreds(filepath.Join(*issuerPath, "key.pem"), filepath.Join(*issuerPath, "crt.pem"))
	if err != nil {
		log.Fatalf("Failed to read CA from %s: %s", *issuerPath, err)
	}

	expectedName := fmt.Sprintf("identity.%s.%s", *controllerNS, *trustDomain)
	if err := creds.Crt.Verify(trustAnchors, expectedName); err != nil {
		log.Fatalf("Failed to verify issuer credentials for '%s' with trust anchors: %s", expectedName, err)
	}

	ca := tls.NewCA(*creds, tls.Validity{
		ClockSkewAllowance: *clockSkewAllowance,
		Lifetime:           *issuanceLifetime,
	})
	if err != nil {
		log.Fatalf("Failed to read issuer credentials from %s: %s", *issuerPath, err)
	}

	k8s, err := k8s.NewClientSet(*kubeConfigPath)
	if err != nil {
		log.Fatalf("Failed to load kubeconfig: %s: %s", *kubeConfigPath, err)
	}
	v, err := idctl.NewK8sTokenValidator(k8s, dom)
	if err != nil {
		log.Fatalf("Failed to initialize identity service: %s", err)
	}

	svc := identity.NewService(v, ca)

	go admin.StartServer(*adminAddr)
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %s", *addr, err)
	}

	srv := grpc.NewServer()
	identity.Register(srv, svc)
	go func() {
		log.Infof("starting gRPC server on %s", *addr)
		srv.Serve(lis)
	}()
	<-stop
	log.Infof("shutting down gRPC server on %s", *addr)
	srv.GracefulStop()
}
