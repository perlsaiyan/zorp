package mailsync

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif" // formats accepted inline
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/markdown"
)

// maxSendImage is the largest image a Markdown message can show inline.
const maxSendImage = 10 << 20

// inlineImage is a local image a Markdown message shows, sent as an inline
// attachment its HTML refers to by content ID.
type inlineImage struct {
	ref string // as written in the message
	cid string
	fileAttachment
}

// ReadInlineImage reads an image a Markdown message refers to, as in
// ![diagram](~/pics/x.png). Only PNG, JPEG and GIF files are accepted,
// checked by their content and not just their name: a reply's quoted text
// comes from someone else, and must not be able to attach other files.
func ReadInlineImage(ref string) (data []byte, contentType string, err error) {
	path := config.ExpandHome(ref)
	if !filepath.IsAbs(path) {
		return nil, "", fmt.Errorf("image %s: use a full path (or ~/...)", ref)
	}
	st, err := os.Stat(path)
	switch {
	case err != nil:
		return nil, "", fmt.Errorf("image %s: %w", ref, err)
	case !st.Mode().IsRegular():
		return nil, "", fmt.Errorf("image %s is not a file", ref)
	case st.Size() > maxSendImage:
		return nil, "", fmt.Errorf("image %s is larger than %d MB; attach it instead", ref, maxSendImage>>20)
	}
	if data, err = os.ReadFile(path); err != nil {
		return nil, "", err
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("image %s isn't a PNG, JPEG or GIF image", ref)
	}
	return data, "image/" + format, nil
}

// inlineImages reads the local images a Markdown message shows.
func inlineImages(mine string) ([]inlineImage, error) {
	var out []inlineImage
	seen := map[string]bool{}
	for _, ref := range markdown.LocalImages(mine) {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		data, ct, err := ReadInlineImage(ref)
		if err != nil {
			return nil, err
		}
		var nonce [6]byte
		rand.Read(nonce[:])
		out = append(out, inlineImage{
			ref:            ref,
			cid:            fmt.Sprintf("image%d.%s@zorp", len(out)+1, hex.EncodeToString(nonce[:])),
			fileAttachment: fileAttachment{name: filepath.Base(config.ExpandHome(ref)), contentType: ct, data: data},
		})
	}
	return out, nil
}

// inlineSrc maps an image reference to its cid: URL.
func inlineSrc(imgs []inlineImage) func(string) string {
	return func(ref string) string {
		for _, im := range imgs {
			if im.ref == ref {
				return "cid:" + im.cid
			}
		}
		return ""
	}
}

func inlineNames(imgs []inlineImage) []string {
	out := make([]string, 0, len(imgs))
	for _, im := range imgs {
		out = append(out, im.name)
	}
	return out
}
