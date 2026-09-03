// The colorscheme provider serves the ryostore colorschemes catalogue as the
// Themes category. It installs a scheme install-only into the shell's theme
// library (dataHome/ryoku/themes/<id>), where the shell daemon converts it into a
// live palette and the Color-scheme picker (Super+W / Hub) applies it. Each
// registry entry carries its Noctalia dark/light palette inline, so an install is
// a cache-only copy that works offline; the catalogue's preview art is fetched
// best-effort beside it so the picker's card shows the same image the store did.
// The entry's provider drives the store's per-provider subtab strip.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const colorschemeRegistryPath = "colorschemes/registry.json"

type colorschemeEntry struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Provider string          `json:"provider"`
	Path     string          `json:"path"`
	Accent   string          `json:"accent,omitempty"`
	Surface  string          `json:"surface,omitempty"`
	Source   string          `json:"source,omitempty"`
	Preview  string          `json:"preview,omitempty"`
	Dark     json.RawMessage `json:"dark,omitempty"`
	Light    json.RawMessage `json:"light,omitempty"`
}

type colorschemeRegistry struct {
	Version int                `json:"version"`
	Themes  []colorschemeEntry `json:"themes"`
}

type colorschemeProvider struct {
	cache      *Cache
	base       string
	libraryDir string
	activeName func() string
}

func newColorschemeProvider(cache *Cache) colorschemeProvider {
	if cache == nil {
		cache = newCache()
	}
	return colorschemeProvider{
		cache:      cache,
		base:       cache.base,
		libraryDir: filepath.Join(dataHome(), "ryoku", "themes"),
		activeName: activeSchemeName,
	}
}

func (colorschemeProvider) Category() Category {
	return Category{
		ID:          "colorschemes",
		Name:        "Themes",
		Group:       "wear",
		Description: "Color schemes for the desktop palette, from every provider.",
	}
}

func (p colorschemeProvider) Load(ctx context.Context, refresh bool) ([]Item, SourceState, error) {
	raw, state, err := p.cache.Fetch(ctx, colorschemeRegistryPath, refresh)
	if err != nil {
		return nil, state, err
	}
	var reg colorschemeRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, state, fmt.Errorf("parse colorscheme registry: %w", err)
	}
	active := p.activeName()
	items := make([]Item, 0, len(reg.Themes))
	for _, e := range reg.Themes {
		if !validComponent(e.ID) {
			return nil, state, fmt.Errorf("invalid colorscheme id %q", e.ID)
		}
		provider := e.Provider
		if provider == "" {
			provider = "Community"
		}
		installed := isRegularFile(filepath.Join(p.libraryDir, e.ID, "scheme.json"))
		tags := make([]string, 0, 2)
		if len(e.Dark) > 0 {
			tags = append(tags, "dark")
		}
		if len(e.Light) > 0 {
			tags = append(tags, "light")
		}
		items = append(items, Item{
			ID:        e.ID,
			Category:  "colorschemes",
			Name:      e.Name,
			Summary:   provider,
			Art:       resolveAsset(p.base, e.Path, e.Preview),
			Author:    provider,
			Accent:    e.Accent,
			Surface:   e.Surface,
			Tags:      tags,
			Installed: installed,
			Active:    installed && e.ID == active,
			Metadata:  map[string]any{"provider": provider},
		})
	}
	return items, state, nil
}

func (p colorschemeProvider) Install(ctx context.Context, id string) error {
	if !validComponent(id) {
		return fmt.Errorf("bad colorscheme id %q", id)
	}
	raw, _, err := p.cache.Fetch(ctx, colorschemeRegistryPath, false)
	if err != nil {
		return err
	}
	var reg colorschemeRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return fmt.Errorf("parse colorscheme registry: %w", err)
	}
	var entry *colorschemeEntry
	for i := range reg.Themes {
		if reg.Themes[i].ID == id {
			entry = &reg.Themes[i]
			break
		}
	}
	if entry == nil {
		return fmt.Errorf("colorscheme %q is not in the store", id)
	}

	// The registry embeds the palette; assemble the library scheme.json from it so
	// an install needs no second fetch and works from the cache offline.
	scheme := map[string]json.RawMessage{}
	if len(entry.Dark) > 0 {
		scheme["dark"] = entry.Dark
	}
	if len(entry.Light) > 0 {
		scheme["light"] = entry.Light
	}
	if len(scheme) == 0 {
		return fmt.Errorf("colorscheme %q carries no palette", id)
	}
	schemeBytes, err := json.MarshalIndent(scheme, "", "  ")
	if err != nil {
		return err
	}
	meta := map[string]any{"label": entry.Name, "provider": entry.Provider}
	if entry.Source != "" {
		meta["source"] = entry.Source
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}

	dst := filepath.Join(p.libraryDir, id)
	unlock, err := lockTree(dst)
	if err != nil {
		return err
	}
	defer unlock()
	stage, err := os.MkdirTemp(filepath.Dir(dst), ".ryostore-stage-"+id+"-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := atomicWrite(filepath.Join(stage, "scheme.json"), schemeBytes, 0o644); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(stage, "meta.json"), metaBytes, 0o644); err != nil {
		return err
	}
	// The catalogue's preview art goes beside the scheme as preview.<ext>: the
	// Color-scheme picker shows that image on the card when the folder carries
	// one and draws the palette pills otherwise. Fetched through the asset cache,
	// so the store's own card and the install share one download, and a failure
	// (offline, upstream 404) still installs the scheme; only the art is missing.
	if remoteAsset(entry.Preview) {
		cached := cachedAssetPath(entry.Preview)
		if !isRegularFile(cached) {
			if err := os.MkdirAll(assetCacheDir(), 0o755); err != nil {
				cached = ""
			} else if err := downloadAsset(ctx, p.cache.client, entry.Preview, cached); err != nil {
				cached = ""
			}
		}
		if cached != "" {
			if ext := strings.ToLower(filepath.Ext(cached)); ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" {
				if b, err := os.ReadFile(cached); err == nil {
					_ = atomicWrite(filepath.Join(stage, "preview"+ext), b, 0o644)
				}
			}
		}
	}
	return replaceTree(stage, dst, nil)
}

func (p colorschemeProvider) Remove(ctx context.Context, id string) error {
	if !validComponent(id) {
		return fmt.Errorf("bad colorscheme id %q", id)
	}
	dst := filepath.Join(p.libraryDir, id)
	if !isRegularFile(filepath.Join(dst, "scheme.json")) {
		return fmt.Errorf("colorscheme %q is not installed", id)
	}
	unlock, err := lockTree(dst)
	if err != nil {
		return err
	}
	defer unlock()
	return os.RemoveAll(dst)
}

// activeSchemeName reads the applied scheme (shell.json theme.theme). It is empty
// for the dynamic variants, so no library scheme reads as active until it is worn
// through the Color-scheme picker; install alone never activates.
func activeSchemeName() string {
	raw, err := os.ReadFile(filepath.Join(configHome(), "ryoku", "shell.json"))
	if err != nil {
		return ""
	}
	var doc struct {
		Theme struct {
			Theme string `json:"theme"`
		} `json:"theme"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return doc.Theme.Theme
}
