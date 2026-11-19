package main

func downloadGallery(count int, download func(int) error, progress func(done, total int)) error {
	if progress != nil {
		progress(0, count)
	}
	results := make(chan error, 6)
	next, active, done := 0, 0, 0
	start := func() {
		i := next
		next++
		active++
		go func() { results <- download(i) }()
	}
	for next < count && active < cap(results) {
		start()
	}
	var firstErr error
	for active > 0 {
		err := <-results
		active--
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if err == nil {
			done++
			if progress != nil {
				progress(done, count)
			}
		}
		if firstErr == nil && next < count {
			start()
		}
	}
	return firstErr
}
