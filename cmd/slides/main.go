// Command slides turns a question file into a single offline HTML file:
// a backup for running the poll by show of hands if the app is unreachable.
//
//	go run ./cmd/slides -o backup.html questions.json
//
// Open it in any browser (no network needed). → / Space / PageDown go
// forward, ← / PageUp go back, F toggles full screen. Printing gives one
// slide per page.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"os"
)

var defaultChoices = []string{"Fine", "Fine with reservations", "Uncomfortable", "Unacceptable"}

type questionFile struct {
	Title     string   `json:"title"`
	Choices   []string `json:"choices"`
	Questions []struct {
		Text   string `json:"text"`
		Detail string `json:"detail"`
	} `json:"questions"`
}

func main() {
	out := flag.String("o", "backup-slides.html", "output file")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: slides [-o out.html] questions.json")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	data, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	var qf questionFile
	if err := json.Unmarshal(data, &qf); err != nil {
		log.Fatalf("%s: %v (check it with the host page's upload, which lists every problem)", flag.Arg(0), err)
	}
	if len(qf.Questions) == 0 {
		log.Fatalf("%s has no questions", flag.Arg(0))
	}
	if len(qf.Choices) == 0 {
		qf.Choices = defaultChoices
	}
	var buf bytes.Buffer
	if err := page.Execute(&buf, qf); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s: title slide + %d questions\n", *out, len(qf.Questions))
}

var page = template.Must(template.New("slides").Funcs(template.FuncMap{
	"inc": func(i int) int { return i + 1 },
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · backup slides</title>
<style>
  :root { --ink: #1b1b19; --muted: #5d5c57; --line: #dcdad3; --bg: #fbfaf7; --accent: #256abf; }
  * { box-sizing: border-box; }
  html, body { margin: 0; height: 100%; background: var(--bg); color: var(--ink);
    font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; }
  .slide { display: none; height: 100vh; padding: 6vh 7vw; flex-direction: column; justify-content: center; }
  .slide.active { display: flex; }
  .eyebrow { color: var(--muted); font-size: 2.4vh; letter-spacing: .04em; text-transform: uppercase; margin: 0 0 2vh; }
  h1 { font-size: 6vh; line-height: 1.15; margin: 0 0 2vh; font-weight: 650; }
  h2 { font-size: 4.6vh; line-height: 1.2; margin: 0 0 1.5vh; font-weight: 600; max-width: 60ch; }
  .detail { font-size: 2.8vh; color: var(--muted); margin: 0 0 4vh; max-width: 70ch; line-height: 1.4; }
  .lead { font-size: 3vh; color: var(--muted); }
  ol { list-style: none; padding: 0; margin: 0; display: grid; gap: 1.6vh; }
  li { display: flex; align-items: center; gap: 2.4vw; font-size: 3.6vh; padding: 1.6vh 2vw;
    border: 1px solid var(--line); border-radius: 1.2vh; background: #fff; }
  .n { flex: none; width: 7vh; height: 7vh; border-radius: 50%; display: grid; place-items: center;
    background: var(--accent); color: #fff; font-weight: 700; font-size: 3.6vh; }
  .how { margin-top: 4vh; font-size: 2.6vh; color: var(--muted); }
  .hint { position: fixed; right: 2vw; bottom: 2vh; color: var(--muted); font-size: 1.6vh; }
  @media print {
    @page { size: landscape; margin: 0; }
    .slide { display: flex !important; height: 100vh; page-break-after: always; break-after: page; }
    .hint { display: none; }
  }
</style>
</head>
<body>
<section class="slide active">
  <p class="eyebrow">Backup: show of hands</p>
  <h1>{{.Title}}</h1>
  <p class="lead">For each question, hold up the number of fingers for your answer:</p>
  <ol>{{range $i, $c := .Choices}}<li><span class="n">{{inc $i}}</span>{{$c}}</li>{{end}}</ol>
</section>
{{range $i, $q := .Questions}}
<section class="slide">
  <p class="eyebrow">Question {{inc $i}} of {{len $.Questions}}</p>
  <h2>{{$q.Text}}</h2>
  {{if $q.Detail}}<p class="detail">{{$q.Detail}}</p>{{end}}
  <ol>{{range $j, $c := $.Choices}}<li><span class="n">{{inc $j}}</span>{{$c}}</li>{{end}}</ol>
  <p class="how">Hold up 1–{{len $.Choices}} fingers.</p>
</section>
{{end}}
<div class="hint">← → to move · F full screen</div>
<script>
  const slides = document.querySelectorAll(".slide");
  let i = 0;
  const show = (n) => { slides[i].classList.remove("active"); i = Math.max(0, Math.min(slides.length - 1, n)); slides[i].classList.add("active"); };
  document.addEventListener("keydown", (e) => {
    if (["ArrowRight", "PageDown", " ", "Enter"].includes(e.key)) show(i + 1);
    else if (["ArrowLeft", "PageUp"].includes(e.key)) show(i - 1);
    else if (e.key === "f" || e.key === "F") document.fullscreenElement ? document.exitFullscreen() : document.documentElement.requestFullscreen();
    else return;
    e.preventDefault();
  });
  document.addEventListener("click", () => show(i + 1));
</script>
</body>
</html>
`))
