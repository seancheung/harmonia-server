package app

import (
	"bytes"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"

	"github.com/harmonia/harmonia-server/internal/airplay"
)

// Reuse the scanner's resolved embedded/external artwork, not its extension:
// legacy cache entries can contain PNG bytes under a .jpg filename.
func remoteArtwork(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > airplay.MaxArtworkBytes {
		return nil, errors.New("cover must be a regular image file no larger than 8 MiB")
	}
	b, err := io.ReadAll(io.LimitReader(f, airplay.MaxArtworkBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > airplay.MaxArtworkBytes {
		return nil, errors.New("cover exceeds 8 MiB")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if (format != "jpeg" && format != "png") || config.Width <= 0 || config.Height <= 0 {
		return nil, errors.New("unsupported cover format")
	}
	return b, nil
}
