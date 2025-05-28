package downloader

import "log/slog"

type VideoDownloader interface {
	MatchURL(url string) (ok bool)
	Download(url string) (file string, err error)
}

type DownloaderRegistry struct {
	downloaders []VideoDownloader
}

func (d *DownloaderRegistry) Add(v VideoDownloader) {
	d.downloaders = append(d.downloaders, v)
}

func (d *DownloaderRegistry) HasMatch(url string) (ok bool) {
	for _, dl := range d.downloaders {
		if dl.MatchURL(url) {
			return true
		}
	}
	return false
}

func (d *DownloaderRegistry) Download(url string) (file string, err error) {
	for _, dl := range d.downloaders {
		if dl.MatchURL(url) {
			file, err = dl.Download(url)
			if err != nil {
				slog.Error("error downloading", "downloader", dl, "url", url, "err", err)
				continue
			}
			return file, nil
		}
	}
	return "", nil
}
