package bot

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/canary"
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

	{{if .Items}}
		<h2>Canary links</h2>
		<p>
		{{range .Summary}}<strong>{{.Site}}</strong> {{.OK}} ok{{if .NoVideo}}, {{.NoVideo}} no video{{end}}{{if .Failed}}, <mark>{{.Failed}} failed</mark>{{end}}{{if .Pending}}, {{.Pending}} waiting{{end}} &nbsp; {{end}}
		</p>
		<table>
			<thead><tr><th>Tested</th><th>Result</th><th>Site</th><th>Via</th><th>Took</th><th>Link</th><th>Trace</th></tr></thead>
			<tbody>
			{{range .Items}}
				<tr>
					<td title="{{.TestedAt}}">{{.TestedAgo}}</td>
					<td>{{if eq .Status "failed"}}<mark>failed</mark>{{else}}{{.Status}}{{end}}</td>
					<td>{{.Site}}</td>
					<td>{{if .Extractor}}{{.Extractor}}{{else}}&ndash;{{end}}</td>
					<td>{{.Took}}</td>
					<td>
						<a href="{{.URL}}" target="_blank" rel="noopener noreferrer nofollow">{{.ShortURL}}</a>
						<br><small>{{.Context}} &middot; {{.Score}} points &middot; found {{.FoundAgo}}{{if .Permalink}} &middot; <a href="{{.Permalink}}" target="_blank" rel="noopener noreferrer nofollow">thread</a>{{end}}</small>
						{{if .Error}}<br><small>{{.ShortError}}</small>{{end}}
					</td>
					<td>{{if .TraceLink}}<a href="{{.TraceLink}}" target="_blank" rel="noopener noreferrer">{{.ShortTrace}}</a>{{else if .ShortTrace}}<small>{{.ShortTrace}}</small>{{else}}&ndash;{{end}}</td>
				</tr>
			{{end}}
			</tbody>
		</table>
	{{end}}

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
	Input   string
	URLs    []string
	Error   string
	Items   []itemRow
	Summary []siteSummary
}

type itemRow struct {
	canary.Item
	TestedAgo  string
	FoundAgo   string
	Took       string
	ShortURL   string
	ShortError string
	ShortTrace string
	TraceLink  string
}

type siteSummary struct {
	Site                         string
	OK, NoVideo, Failed, Pending int
}

// maxItemRows is how many canary links the page lists.
const maxItemRows = 100

func ago(t time.Time) string {
	if t.IsZero() {
		return "–"
	}
	return time.Since(t).Round(time.Second).String() + " ago"
}

func truncateTo(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// buildItems orders the links with the latest tested first, then the ones
// still waiting, best score first, and totals them per site.
func buildItems(items []canary.Item, grafanaURL string) ([]itemRow, []siteSummary) {
	bySite := map[string]*siteSummary{}
	for _, it := range items {
		s := bySite[it.Site]
		if s == nil {
			s = &siteSummary{Site: it.Site}
			bySite[it.Site] = s
		}
		switch it.Status {
		case "ok":
			s.OK++
		case "no_video":
			s.NoVideo++
		case "failed":
			s.Failed++
		default:
			s.Pending++ // waiting or being tested
		}
	}

	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.TestedAt.IsZero() != b.TestedAt.IsZero() {
			return !a.TestedAt.IsZero()
		}
		if !a.TestedAt.IsZero() {
			return a.TestedAt.After(b.TestedAt)
		}
		return a.Score > b.Score
	})
	if len(items) > maxItemRows {
		items = items[:maxItemRows]
	}

	rows := make([]itemRow, len(items))
	for i, it := range items {
		row := itemRow{
			Item:       it,
			TestedAgo:  ago(it.TestedAt),
			FoundAgo:   ago(it.FoundAt),
			ShortURL:   truncateTo(it.URL, 70),
			ShortError: truncateTo(it.Error, 300),
			ShortTrace: truncateTo(it.TraceID, 8),
		}
		if !it.TestedAt.IsZero() {
			row.Took = it.Duration.Round(10 * time.Millisecond).String()
		}
		if grafanaURL != "" && it.TraceID != "" {
			row.TraceLink = traceLink(grafanaURL, it.TraceID)
		}
		rows[i] = row
	}

	summary := make([]siteSummary, 0, len(bySite))
	for _, s := range bySite {
		summary = append(summary, *s)
	}
	sort.Slice(summary, func(i, j int) bool { return summary[i].Site < summary[j].Site })
	return rows, summary
}

// traceLink is a Grafana Explore url that opens the trace in the Tempo datasource.
func traceLink(grafanaURL, traceID string) string {
	panes, _ := json.Marshal(map[string]any{"a": map[string]any{
		"datasource": "tempo",
		"queries": []any{map[string]any{
			"datasource": map[string]string{"uid": "tempo"},
			"query":      traceID,
			"queryType":  "traceql",
			"refId":      "A",
		}},
		"range": map[string]string{"from": "now-7d", "to": "now"},
	}})
	return strings.TrimSuffix(grafanaURL, "/") + "/explore?schemaVersion=1&panes=" + url.QueryEscape(string(panes))
}

var tmpl = template.Must(
	template.New("page").
		Funcs(template.FuncMap{
			"hasSuffix": strings.HasSuffix,
		}).
		Parse(page),
)

// SimpleServer is a debug page: paste a url to run a reupload end to end, and
// see the canary's links and results. items and grafanaURL are optional.
func SimpleServer(reup *preview.Reuploader, items func() []canary.Item, grafanaURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		data := pageData{}
		if items != nil {
			data.Items, data.Summary = buildItems(items(), grafanaURL)
		}

		switch r.Method {
		case "GET":
			err := tmpl.Execute(w, data)
			if err != nil {
				fmt.Printf("template execute error: %v\n", err)
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
				fmt.Printf("template execute error: %v\n", err)
			}
		default:
			http.NotFound(w, r)
		}
	})
}
