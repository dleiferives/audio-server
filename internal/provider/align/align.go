package align

import (
	"context"
	"os"
	"strings"

	align "github.com/dleiferives/MFA-go"
)

type Provider struct {
	mfa   *align.Provider
	langs map[string]LanguageModel
}

type LanguageModel struct {
	Acoustic   string `yaml:"acoustic"`
	Dictionary string `yaml:"dictionary"`
	G2P        string `yaml:"g2p,omitempty"`
}

func NewAlignProvider(mfaEnv, workDir, binDir string, langs map[string]LanguageModel) *Provider {
	p := align.New(align.Config{
		MFAEnv:  mfaEnv,
		WorkDir: workDir,
		BinDir:  binDir,
	})
	return &Provider{mfa: p, langs: langs}
}

func (p *Provider) Align(ctx context.Context, audio []byte, transcript, language string) (any, error) {
	lang, ok := p.langs[normalizeLang(language)]
	if !ok {
		lang, ok = p.langs["en"]
		if !ok {
			return nil, os.ErrNotExist
		}
	}

	g2pPath := lang.G2P
	if g2pPath != "" && !strings.HasPrefix(g2pPath, "/") {
		cwd, _ := os.Getwd()
		g2pPath = cwd + "/" + g2pPath
	}

	return p.mfa.Align(ctx, audio, transcript, lang.Acoustic, lang.Dictionary, g2pPath)
}

func (p *Provider) Languages() []string {
	var langs []string
	for k := range p.langs {
		langs = append(langs, k)
	}
	return langs
}

func (p *Provider) HasLanguage(language string) bool {
	_, ok := p.langs[normalizeLang(language)]
	return ok
}

func normalizeLang(l string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(l), "_", "-"))
}
