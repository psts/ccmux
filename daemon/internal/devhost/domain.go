package devhost

import (
	"context"
	"crypto/tls"
	"log"
	"path/filepath"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
)

// magicState is one certmagic lifecycle, keyed by (domain, token) so a settings
// change tears the old one down and starts fresh.
type magicState struct {
	key    string
	cache  *certmagic.Cache
	config *certmagic.Config
	cancel context.CancelFunc
}

// ensureCertLocked (re)builds the certmagic config when the (domain, token)
// pair changed and kicks off async issuance of the *.<domain> wildcard.
// DNS-01 via Cloudflare is the only challenge that works for a tailnet-only
// host; propagation checks are pinned to public resolvers because the daemon
// host's own resolver is MagicDNS.
func (s *Server) ensureCertLocked(domain, token string) {
	key := domain + "\x00" + token
	if s.magic != nil && s.magic.key == key {
		return
	}
	s.teardownMagicLocked()
	if token == "" {
		s.setCertStatus("error: no Cloudflare token configured")
		return
	}

	solver := &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{
		DNSProvider: &cloudflare.Provider{APIToken: token},
		Resolvers:   []string{"1.1.1.1:53", "8.8.8.8:53"},
	}}
	var cfg *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return cfg, nil },
	})
	cfg = certmagic.New(cache, certmagic.Config{
		Storage: &certmagic.FileStorage{Path: filepath.Join(s.dataDir, "certmagic")},
	})
	issuer := certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
		CA:          certmagic.LetsEncryptProductionCA,
		Agreed:      true,
		DNS01Solver: solver,
	})
	cfg.Issuers = []certmagic.Issuer{issuer}

	ctx, cancel := context.WithCancel(s.ctx)
	s.magic = &magicState{key: key, cache: cache, config: cfg, cancel: cancel}
	s.magicCfg.Store(cfg)
	s.setCertStatus("pending")
	wildcard := "*." + domain
	go func() {
		if err := cfg.ManageSync(ctx, []string{wildcard}); err != nil {
			log.Printf("devhost: cert %s: %v", wildcard, err)
			s.setCertStatus("error: " + err.Error())
			return
		}
		log.Printf("devhost: cert %s ready", wildcard)
		s.setCertStatus("ready")
	}()
}

// teardownMagicLocked stops the active certmagic lifecycle (renewals included).
func (s *Server) teardownMagicLocked() {
	if s.magic == nil {
		return
	}
	s.magic.cancel()
	s.magic.cache.Stop()
	s.magic = nil
	s.magicCfg.Store(nil)
}

func (s *Server) setCertStatus(v string) { s.certStatus.Store(&v) }

// TLSConfig returns the SNI dispatcher for the daemon's tsnet :443 listener:
// names under the dev domain are served with the certmagic wildcard, everything
// else (the node's own ts.net name) with the tailnet cert via tsGetCert.
func (s *Server) TLSConfig(tsGetCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{
		GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if cfg := s.magicCfg.Load(); cfg != nil && s.underDevDomain(hi.ServerName) {
				return cfg.GetCertificate(hi)
			}
			return tsGetCert(hi)
		},
	}
}
