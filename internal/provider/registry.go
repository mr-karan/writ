package provider

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/schema"
)

const (
	metadataTimeout = 30 * time.Second
	maxMetadata     = 8 << 20
)

// registry speaks the provider registry protocol of one host:
// https://opentofu.org/docs/internals/provider-registry-protocol/.
type registry struct {
	http *http.Client
}

type registryVersions struct {
	Versions []registryVersion `json:"versions"`
}

type registryVersion struct {
	Version   string             `json:"version"`
	Protocols []string           `json:"protocols"`
	Platforms []registryPlatform `json:"platforms"`
}

type registryPlatform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type registryDownload struct {
	Protocols   []string `json:"protocols"`
	Filename    string   `json:"filename"`
	DownloadURL string   `json:"download_url"`
	Shasum      string   `json:"shasum"`
}

// release is what install needs from the registry for one platform.
type release struct {
	url     *url.URL
	archive [32]byte
}

// release looks up src at exactly version for platform and checks that it
// serves a plugin protocol Writ speaks.
func (r registry) release(ctx context.Context, src schema.Source, version, platform string) (release, error) {
	base, err := r.providersBase(ctx, host(src))
	if err != nil {
		return release{}, err
	}
	var vs registryVersions
	versionsURL := base.JoinPath(src.Namespace, src.Type, "versions")
	if err := r.getJSON(ctx, versionsURL, &vs); err != nil {
		return release{}, err
	}
	if _, err := selectVersion(vs, src, version, platform); err != nil {
		return release{}, err
	}
	goos, arch, _ := strings.Cut(platform, "_")
	var dl registryDownload
	dlURL := base.JoinPath(src.Namespace, src.Type, version, "download", goos, arch)
	if err := r.getJSON(ctx, dlURL, &dl); err != nil {
		return release{}, err
	}
	if !speaksProtocol(dl.Protocols) {
		return release{}, fmt.Errorf("%s %s serves plugin protocols %v; writ speaks 5 and 6", src, version, dl.Protocols)
	}
	archive, err := hex.DecodeString(dl.Shasum)
	if err != nil || len(archive) != 32 {
		return release{}, fmt.Errorf("%s %s: registry shasum %q is not a SHA-256", src, version, dl.Shasum)
	}
	u, err := dlURL.Parse(dl.DownloadURL)
	if err != nil {
		return release{}, fmt.Errorf("%s %s: download url: %w", src, version, err)
	}
	if u.Scheme != "https" {
		return release{}, fmt.Errorf("%s %s: refusing non-https download url %s", src, version, u.Redacted())
	}
	rel := release{url: u}
	copy(rel.archive[:], archive)
	return rel, nil
}

// selectVersion finds exactly version in the registry's list and checks that
// it is built for platform and serves protocol 5 or 6.
func selectVersion(vs registryVersions, src schema.Source, version, platform string) (registryVersion, error) {
	i := slices.IndexFunc(vs.Versions, func(v registryVersion) bool { return v.Version == version })
	if i < 0 {
		return registryVersion{}, fmt.Errorf("%s has no version %s in %s", src, version, host(src))
	}
	v := vs.Versions[i]
	goos, arch, _ := strings.Cut(platform, "_")
	if !slices.Contains(v.Platforms, registryPlatform{OS: goos, Arch: arch}) {
		return registryVersion{}, fmt.Errorf("%s %s has no build for %s", src, version, platform)
	}
	if !speaksProtocol(v.Protocols) {
		return registryVersion{}, fmt.Errorf("%s %s serves plugin protocols %v; writ speaks 5 and 6", src, version, v.Protocols)
	}
	return v, nil
}

func speaksProtocol(protocols []string) bool {
	return slices.ContainsFunc(protocols, func(p string) bool {
		major, _, _ := strings.Cut(p, ".")
		return major == "5" || major == "6"
	})
}

func host(src schema.Source) string {
	if src.Host == "" {
		return schema.DefaultHost
	}
	return src.Host
}

// providersBase reads the host's service discovery document for the
// providers.v1 base URL.
func (r registry) providersBase(ctx context.Context, h string) (*url.URL, error) {
	wellKnown := &url.URL{Scheme: "https", Host: h, Path: "/.well-known/terraform.json"}
	var doc struct {
		Providers string `json:"providers.v1"`
	}
	if err := r.getJSON(ctx, wellKnown, &doc); err != nil {
		return nil, err
	}
	if doc.Providers == "" {
		return nil, fmt.Errorf("%s does not offer a provider registry", h)
	}
	base, err := wellKnown.Parse(doc.Providers)
	if err != nil {
		return nil, fmt.Errorf("%s: providers.v1: %w", h, err)
	}
	return base, nil
}

func (r registry) getJSON(ctx context.Context, u *url.URL, v any) error {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: %s", u.Redacted(), resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadata)).Decode(v); err != nil {
		return fmt.Errorf("get %s: %w", u.Redacted(), err)
	}
	return nil
}
