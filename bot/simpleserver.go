package bot

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/robertkozin/discord-video-preview-bot/preview"
)

var page = `<!doctype html>
<html lang="en">
<head>
<title>Discord Video Preview</title>
<meta charset="utf-8">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/water.css@2/out/water.css">
</head>
<body>
	<h1>Discord Video Preview Tester</h1>

	<form method="POST">
		<label for="input">Video URL</label>
		<input type="url" id="input" name="input" placeholder="https://www.tiktok.com/@example/video/..." value="{{.Input}}" style="width: 100%">
		<button type="submit">Test Reupload</button>
	</form>

	{{if .Error}}
		<h2>Error</h2>
		<pre><code>{{.Error}}</code></pre>
	{{end}}

	{{if .URLs}}
		<h2>Result</h2>
		{{range .URLs}}
			<p><a href="{{.}}" target="_blank">{{.}}</a></p>
			{{if or (hasSuffix . ".mp4")}}
				<video controls width="400">
					<source src="{{.}}" type="video/mp4">
					Your browser does not support the video tag.
				</video>
			{{else if or (hasSuffix . ".jpg") (hasSuffix . ".jpeg") (hasSuffix . ".png")}}
				<img src="{{.}}" alt="Media" style="max-width: 400px; height: auto;">
			{{end}}
		{{end}}
	{{end}}
</body>
</html>`

type pageData struct {
	Input string
	URLs  []string
	Error string
}

var tmpl = template.Must(
	template.New("page").
		Funcs(template.FuncMap{
			"hasSuffix": strings.HasSuffix,
		}).
		Parse(page),
)

func SimpleServer(reup *preview.Reuploader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		data := pageData{}

		switch r.Method {
		case "GET":
			err := tmpl.Execute(w, data)
			if err != nil {
				fmt.Printf("template execute error: %w\n", err)
			}
		case "POST":
			data.Input = r.FormValue("input")
			urls, err := reup.Reupload(r.Context(), data.Input)
			if err != nil {
				data.Error = err.Error()
			} else {
				data.URLs = urls
			}

			err = tmpl.Execute(w, data)
			if err != nil {
				fmt.Printf("template execute error: %w", err)
			}
		default:
			http.NotFound(w, r)
		}
	})
}
