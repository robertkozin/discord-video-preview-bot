package downloader

type Template struct {
}

func (c *Template) String() string {
	return "template"
}

func (c *Template) MatchURL(url string) bool {
	return simpleURLMatch(url, []string{
		"example.com/*",
	})
}

func (c *Template) Download(url string) (string, error) {
	return "", nil
}
