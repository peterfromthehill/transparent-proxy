package main

import (
    "io"
    "log"
    "net/http"
    "net"
    "strings"
    "bytes"
    "crypto/rand"
    "crypto/rsa"
    "crypto/tls"
    "crypto/x509"
    "crypto/x509/pkix"
    "encoding/pem"
    "math/big"
    "time"
    "io/ioutil"
    "os"
    "errors"
    "fmt"
)

var caCer *x509.Certificate
var caKey *rsa.PrivateKey

// SSLEntry holds private key and certificate for a created site
type SSLEntry struct {
    certPrivKeyPEM bytes.Buffer
    certPEM bytes.Buffer
}

// SSLCache cached TLS certificates from sites visited in the history
var SSLCache map[string]SSLEntry

func handleHTTP(w http.ResponseWriter, req *http.Request, scheme string) {
    var requestHeader = http.Header{}
    copyHeader(requestHeader, req.Header)

    req.URL.Scheme = scheme
    req.URL.Host = req.Host

    log.Printf("%s => %s %s", req.RemoteAddr, req.Method, req.URL)

    resp, err := http.DefaultTransport.RoundTrip(req)
    if err != nil {
        http.Error(w, err.Error(), http.StatusServiceUnavailable)
        return
    }
    defer resp.Body.Close()
    log.Printf("%s <= %s %d", req.RemoteAddr, resp.Status, resp.ContentLength)
    copyHeader(w.Header(), resp.Header)
    w.WriteHeader(resp.StatusCode)
    io.Copy(w, resp.Body)
}        

func copyHeader(dst, src http.Header) {
    for k, vv := range src {
        if strings.ToLower(k) == "connection" {
            dst.Add(k, "close")
        } else if strings.ToLower(k) == "keep-alive" {
            continue
        } else {
            for _, v := range vv {
                dst.Add(k, v)
            }
        }
    }
}

func main() {
    env := []string{"HTTP_PORT", "HTTPS_PORT", "SSLKEY_FILE", "SSLCERT_FILE"}
    for _, e := range env {
        if os.Getenv(e) == "" {
            panic(e + " dont set")
        }
    }

    log.SetFlags(log.Lshortfile | log.LstdFlags)
    var err error
    caCer, caKey, err = bootstrap(os.Getenv("SSLCERT_FILE"), os.Getenv("SSLKEY_FILE"))
    if err != nil {
        log.Println(err)
        return
    }
    startWebserver(os.Getenv("HTTP_PORT"), os.Getenv("HTTPS_PORT"))
}

func startWebserver(httpPort, httpsPort string) {
    config := &tls.Config{
        GetCertificate: returnCert,
    }

    finish := make(chan bool)

    ln, err := tls.Listen("tcp", ":" + httpsPort, config)
    if err != nil {
        log.Println(err)
        return
    }
    defer ln.Close()

    lm, err := net.Listen("tcp", ":" + httpPort)
    if err != nil {
        log.Println(err)
        return
    }

    go func () {
        err := http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                    handleHTTP(w, r, "https")
            }))
        fmt.Errorf("HTTPS: %s", err)
    }()
    go func () {
        err := http.Serve(lm, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                handleHTTP(w, r, "http")
        }))
        fmt.Errorf("HTTP: %s", err)
    }()
    log.Printf("Webserver startd in Ports %s & %s", httpPort, httpsPort)
    <-finish
}

func sslCacheWatcher(interval time.Duration) {
    for {
        sslCacheWatcher0()
        time.Sleep(interval * time.Second)    
    }
}

func sslCacheWatcher0() {
    for i, v := range SSLCache {
        cer, err := parseX509Cert(v.certPEM.Bytes())
        if err != nil {
            log.Printf("%s: invalid cert!", i)
            continue
        }
        if cer.NotAfter.Before(time.Now().Add(time.Minute * 5)) {
            log.Printf("%s: cert expired, delete it from cache", i)
            delete(SSLCache, i)
            continue
        }
        log.Printf("%s %s\n", i, cer.NotAfter)
    }    
}

func loadPKfromFile(file string) (*rsa.PrivateKey, error) {
    priv, err := ioutil.ReadFile(file)
    if err != nil {
        return nil, err
    }

    key, err := parsePrivateKey(priv)
    if err != nil {
        return nil, fmt.Errorf("%s: %s", file, err)
    }    
    return key, nil
}

func parsePrivateKey(privKey []byte) (*rsa.PrivateKey, error) {
    privPem, _ := pem.Decode(privKey)
    var privPemBytes []byte
    if privPem.Type != "RSA PRIVATE KEY" {
        return nil, errors.New("RSA private key is of the wrong type")
    }

    privPemBytes = privPem.Bytes
    key, err := x509.ParsePKCS1PrivateKey(privPemBytes)
    return key, err
}

func loadX509fromFile(file string) (*x509.Certificate, error) {
    cert, err := ioutil.ReadFile(file)
    if err != nil {
        return nil, err
    }
    cer, err := parseX509Cert(cert)
    if err != nil {
        return nil, fmt.Errorf("%s: %s", file, err)
    }       
    return cer, nil
}

func parseX509Cert(cert []byte) (*x509.Certificate, error) {
    certPool := x509.NewCertPool()
    ok := certPool.AppendCertsFromPEM(cert)
    if !ok {
        return nil, fmt.Errorf("failed to parse root certificate")
    }    
    block, _ := pem.Decode([]byte(cert))
    if block == nil {
        return nil, fmt.Errorf("failed to parse certificate PEM")
    }    
    cer, err := x509.ParseCertificate(block.Bytes)
    return cer, err
}

func bootstrap(tlsCertPath, privateKeyPath string) (*x509.Certificate, *rsa.PrivateKey, error) {    
    cer, err := loadX509fromFile(tlsCertPath)
    if err != nil {
        return nil, nil, err
    }
    key, err := loadPKfromFile(privateKeyPath)
    if err != nil {
        return nil, nil, err
    }
    SSLCache = make(map[string]SSLEntry)
    go sslCacheWatcher(10)    
    return cer, key, nil
}

func findCertinCache(serverName string) (*tls.Certificate, error) {
    if sslEntry, ok := SSLCache[serverName]; ok != false {
        serverCert, err := tls.X509KeyPair(sslEntry.certPEM.Bytes(), sslEntry.certPrivKeyPEM.Bytes())
        if err != nil {
            return nil, err
        }
        return &serverCert, nil
    }
    return nil, fmt.Errorf("%s Cert not found in cache", serverName)
}

func createCertificateTemplate(serverName string, minutes time.Duration) (*x509.Certificate) {
    cert := &x509.Certificate{
        SerialNumber: big.NewInt(2019),
        Subject: pkix.Name{
            CommonName:       serverName,
        },
        NotBefore:    time.Now(),
        NotAfter:     time.Now().Add(time.Minute * minutes),
        SubjectKeyId: []byte{1, 2, 3, 4, 6},
        ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
        KeyUsage:     x509.KeyUsageDigitalSignature,
    }

    if ip := net.ParseIP(serverName); ip != nil {
        cert.IPAddresses = append(cert.IPAddresses, ip)
    } else {
        cert.DNSNames = append(cert.DNSNames, serverName)
    }
    return cert
}

func returnCert(helloInfo *tls.ClientHelloInfo) (*tls.Certificate, error) {
    if serverCert, err := findCertinCache(helloInfo.ServerName); err == nil {
        return serverCert, nil
    }
    // set up our server certificate
    cert := createCertificateTemplate(helloInfo.ServerName, 60*24)

    certPrivKey, err := rsa.GenerateKey(rand.Reader, 4096)
    if err != nil {
        return nil, fmt.Errorf("%s: %s", helloInfo.ServerName, err)
    }

    certBytes, err := x509.CreateCertificate(rand.Reader, cert, caCer, &certPrivKey.PublicKey, caKey)
    if err != nil {
        return nil, fmt.Errorf("%s: %s", helloInfo.ServerName, err)
    }

    certPEM := new(bytes.Buffer)
    pem.Encode(certPEM, &pem.Block{
        Type:  "CERTIFICATE",
        Bytes: certBytes,
    })

    certPrivKeyPEM := new(bytes.Buffer)
    pem.Encode(certPrivKeyPEM, &pem.Block{
        Type:  "RSA PRIVATE KEY",
        Bytes: x509.MarshalPKCS1PrivateKey(certPrivKey),
    })
    
    sslentry := SSLEntry{*certPrivKeyPEM, *certPEM}
    SSLCache[helloInfo.ServerName] = sslentry

    serverCert, err := tls.X509KeyPair(certPEM.Bytes(), certPrivKeyPEM.Bytes())
    if err != nil {
        return nil, fmt.Errorf("%s: %s", helloInfo.ServerName, err)
    }

    return &serverCert, nil
}