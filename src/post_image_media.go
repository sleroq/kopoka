package main

import (
	"bytes"
	"fmt"
	"image"
)

func (m *postMedia) imageURLs() []string {
	if m == nil {
		return nil
	}
	var urls []string
	if len(m.Items) > 0 {
		for _, item := range m.Items {
			u := item.URL
			if item.Type != "photo" {
				u = item.ThumbnailURL
			}
			urls = append(urls, u)
		}
		return urls
	}
	if m.Mosaic != nil {
		urls = append(urls, m.Mosaic.URL)
	} else {
		for _, photo := range m.Photos {
			urls = append(urls, photo.URL)
		}
	}
	for _, video := range m.Videos {
		urls = append(urls, video.ThumbnailURL)
	}
	return urls
}

func (m *postMedia) hasBars() bool {
	return m != nil && (len(m.Photos) > 1 || len(m.Items) > 1)
}

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
