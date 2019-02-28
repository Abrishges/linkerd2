package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	pb "github.com/linkerd/linkerd2-proxy-api/go/identity"
	"github.com/linkerd/linkerd2/pkg/flags"
	"github.com/linkerd/linkerd2/pkg/tls"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

func main() {
	addr := flag.String("addr", "localhost:8083", "address of identity service")
	trustAnchorsPath := flag.String("trust-anchors", "", "path to PEM-encoded trust anchors")
	tokenPath := flag.String("token", "", "path to serviceaccount token")
	name := flag.String("name", "", "identity name")
	dir := flag.String("dir", "", "directory under which credentials are written")
	flags.ConfigureAndParse()

	verify, err := loadVerifier(*trustAnchorsPath)
	if err != nil {
		log.Fatalf("Failed to load trust anchors: %s", err)
	}

	keyPath, csrPath, crtPath, err := checkEndEntityDir(*dir)
	if err != nil {
		log.Fatalf("Invalid end-entity directory: %s", err)
	}

	key, err := generateAndStoreKey(keyPath)
	if err != nil {
		log.Fatal(err.Error())
	}

	csrb, err := generateAndStoreCSR(csrPath, *name, key)
	if err != nil {
		log.Fatal(err.Error())
	}

	if *tokenPath == "" {
		log.Fatalf("-token must be specified")
	}
	tokenb, err := ioutil.ReadFile(*tokenPath)
	if err != nil {
		log.Fatalf("Failed to read token: %s", err)
	}

	certifyReq := pb.CertifyRequest{
		Identity:                  *name,
		CertificateSigningRequest: csrb,
		Token:                     tokenb,
	}

	// TODO replace this with a secure connection to the identity service.
	conn, err := grpc.Dial(*addr, grpc.WithInsecure())
	if err != nil {
		log.Fatal(err.Error())
	}
	client := pb.NewIdentityClient(conn)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	var crt *x509.Certificate
	for {
		// Attempt to reload the token, in case it changed.
		tokenb, err := ioutil.ReadFile(*tokenPath)
		if err != nil {
			log.Errorf("Failed to reload token: %s", err)
		} else {
			certifyReq.Token = tokenb
		}

		rsp, err := client.Certify(context.Background(), &certifyReq)
		if err != nil {
			log.Errorf("Failed to obtain certificate: %s", err)
		} else {
			crt, err = validateAndStoreCrt(rsp, crtPath, verify)
			if err != nil {
				log.Errorf("Failed to validate and store certificate: %s", err)
			}
		}

		var refreshIn time.Duration
		if crt != nil {
			refreshIn = beforeExpiry(crt.NotAfter)
			log.Infof("id=%s; fp=%s; expiry=%s; refresh=%s", *name, fp(crt), crt.NotAfter, refreshIn)
		} else {
			refreshIn = MIN_REFRESH_TIME
			log.Infof("NO CERTIFICATE; refresh=%s", refreshIn)
		}

		select {
		case <-time.NewTimer(refreshIn).C:
			continue
		case <-stop:
			log.Info("Shutting down...")
		}
	}
}

func loadVerifier(path string) (verify x509.VerifyOptions, err error) {
	if path == "" {
		err = errors.New("No trust anchors specified")
		return
	}

	b, err := ioutil.ReadFile(path)
	if err != nil {
		return
	}

	anchors, err := tls.DecodePEMCertPool(string(b))
	if err != nil {
		return
	}

	verify.Roots = anchors
	return
}

// checkEndEntityDir checks that the provided directory path exists and is
// suitable to write key material to, returning the Key, CSR, and Crt paths.
//
// If the directory does not exist or if it has incorrect permissions, we assume
// that the wrong directory was specified instead of trying to create or repair
// the directory. In practice, this directory should be tmpfs so that
// credentials are not written to disk, so we want to be extra sensitive to
// incorrectly specified paths.4
//
// If the key, CSR, and/or Crt paths refer to existing files, it is assumed that
// multiple instances of this process are running, and an error is returned.
func checkEndEntityDir(dir string) (string, string, string, error) {
	if dir == "" {
		return "", "", "", errors.New("No end entity directory specified")
	}

	s, err := os.Stat(dir)
	if err != nil {
		return "", "", "", err
	}
	if !s.IsDir() {
		return "", "", "", fmt.Errorf("Not a directory: %s", dir)
	}
	if s.Mode().Perm() == 0700 {
		return "", "", "", fmt.Errorf("Must have permissions 0700: %s; got %s", dir, s.Mode().Perm())
	}

	keyPath := filepath.Join(dir, "key")
	if err = checkNotExists(keyPath); err != nil {
		return "", "", "", err
	}

	csrPath := filepath.Join(dir, "csr")
	if err = checkNotExists(csrPath); err != nil {
		return "", "", "", err
	}

	crtPath := filepath.Join(dir, "crt.pem")
	if err = checkNotExists(crtPath); err != nil {
		return "", "", "", err
	}

	return keyPath, csrPath, crtPath, nil
}

func checkNotExists(p string) (err error) {
	_, err = os.Stat(p)
	if err == nil {
		err = fmt.Errorf("Already exists: %s", p)
	} else if os.IsNotExist(err) {
		err = nil
	}
	return
}

func generateAndStoreKey(p string) (key *ecdsa.PrivateKey, err error) {
	// Generate a private key and store it read-only (i.e. mostly for debugging). Because the file is read-only
	key, err = tls.GenerateKey()
	if err == nil {
		return
	}

	err = ioutil.WriteFile(p, tls.EncodePrivateKeyP8(key), 0400)
	return
}

func generateAndStoreCSR(p, id string, key *ecdsa.PrivateKey) ([]byte, error) {
	if id == "" {
		return nil, errors.New("A non-empty identity is required")
	}

	// TODO do proper DNS name validation.
	csr := x509.CertificateRequest{DNSNames: []string{id}}
	csrb, err := x509.CreateCertificateRequest(rand.Reader, &csr, key)
	if err != nil {
		return nil, fmt.Errorf("Failed to create CSR: %s", err)
	}

	if err = ioutil.WriteFile(p, csrb, 0400); err != nil {
		return nil, fmt.Errorf("Failed to write CSR: %s", err)
	}

	return csrb, nil
}

func validateAndStoreCrt(rsp *pb.CertifyResponse, crtPath string, verify x509.VerifyOptions) (*x509.Certificate, error) {
	crtb := rsp.GetLeafCertificate()
	if len(crtb) == 0 {
		return nil, errors.New("Certify response does not include a certificate")
	}

	crt, err := x509.ParseCertificate(crtb)
	if err != nil {
		return nil, err
	}

	verify.Intermediates = x509.NewCertPool()
	for _, b := range rsp.GetIntermediateCertificates() {
		c, err := x509.ParseCertificate(b)
		if err != nil {
			return nil, err
		}

		verify.Intermediates.AddCert(c)
	}

	if _, err = crt.Verify(verify); err != nil {
		return nil, err
	}

	if time.Now().Before(crt.NotBefore) {
		return nil, errors.New("Received a certificate that is not yet valid")
	} else if time.Now().After(crt.NotAfter) {
		return nil, errors.New("Received a certificate that has expired")
	}

	return crt, ioutil.WriteFile(crtPath, crtb, 0600)
}

func fp(crt *x509.Certificate) string {
	sum := sha256.Sum256(crt.Raw)
	return strings.ToLower(hex.EncodeToString(sum[:]))
}

const MIN_REFRESH_TIME = 1 * time.Second
const MAX_REFRESH_TIME = 24 * time.Hour

func beforeExpiry(t time.Time) time.Duration {
	expiry := time.Until(t)
	if expiry < MIN_REFRESH_TIME {
		return MIN_REFRESH_TIME
	}

	r := (expiry / time.Second) * (800 * time.Millisecond)
	if r > MAX_REFRESH_TIME {
		return MAX_REFRESH_TIME
	}

	return r
}
