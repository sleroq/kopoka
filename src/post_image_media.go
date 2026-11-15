package main

import (
	"bytes"
	"fmt"
	"image"
)

func (m *postMedia) preparePhotos(assets map[string][]byte) error {
	if len(m.Photos) < 2 {
		return nil
	}
	decoded := make([]image.Image, len(m.Photos))
	for i, photo := range m.Photos {
		img, _, err := image.Decode(bytes.NewReader(assets[photo.URL]))
		if err != nil {
			return fmt.Errorf("decoding photo %s: %w", photo.URL, err)
		}
		decoded[i] = img
	}
	var photos []postImage
	for _, panel := range joinArtPanels(decoded) {
		originals := m.Photos[panel.first : panel.last+1]
		if panel.first == panel.last {
			photos = append(photos, originals...)
			continue
		}
		size := panel.image.Bounds().Size()
		if size.X > 3*size.Y || size.Y > 3*size.X {
			m.Carousel = true
			photos = append(photos, originals...)
			continue
		}
		file, err := mergedPhotoFile(panel.image)
		if err != nil {
			return fmt.Errorf("encoding merged photos: %w", err)
		}
		if len(file.Bytes) == 0 {
			return fmt.Errorf("encoding merged photos: image exceeds size limit")
		}
		key := originals[0].URL + "#merged"
		assets[key] = file.Bytes
		photos = append(photos, postImage{URL: key})
	}
	m.Photos = photos
	m.Carousel = m.Carousel || len(photos) > 4
	return nil
}
