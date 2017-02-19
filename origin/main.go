package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"runtime"
	"strconv"
	"text/template"
	"time"
)

var (
	size    = flag.Int("size", 1000, "How many nodes to build website")
	links   = flag.Int("link", 20, "How many links on each page")
	images  = flag.Int("imgs", 6, "How many images on each page")
	nimg    = flag.Int("nimg", 30, "Total number of images in system")
	content = flag.Int("pars", 40, "How many paragraphs on each page")
	seed    = flag.Int64("seed", 0, "Seed of the random package")
	port    = flag.Int("port", 8080, "What port to run server on")
	inter   = flag.Duration("log", time.Second, "What interval to operate logs on")
	mask    = "%09d"
)

func pad(num int) string { return fmt.Sprintf(mask, num) }

var t = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="en">
	<head>
		<title>Page {{.Title}}</title>
	</head>
	<body>
		<h1>Page {{.Title}}</h1>
		<h2>Links</h2>
		<ul>{{range .Links}}
			<li>
				<a href="/page/{{ . }}">Link {{ . }}</a>
			</li>{{end}}
		</ul>
		<h2>Images</h2>
		<ul>{{range .Images}}
			<li>
				<img src="/img/{{ . }}" alt="Image {{ . }}">
			</li>{{end}}
		</ul>
		<h2>Content</h2>
		<ul>{{range .Content}}
			<li>
				<p>{{ . }}</p>
			</li>{{end}}
		</ul>
	</body>
</html>`))

type server struct {
	g        graph
	imgCache [][]byte
	stat     stats
}

func (s *server) page(w http.ResponseWriter, r *http.Request) {
	// now := time.Now()
	u, err := strconv.Atoi(r.URL.Path[6:]) // 6 = len("/page/")
	if err != nil {
		http.NotFound(w, r)
		s.stat.inc("bad")
		return
	}
	if u > s.g.Size() || u < 0 {
		s.redirect(w, r)
		return
	}

	links := make([]string, len(s.g[u]))
	for i, n := range s.g[u] {
		links[i] = pad(n)
	}

	imgs := make([]string, *images)
	rander := rand.New(rand.NewSource(int64(u)))
	for i := range imgs {
		imgs[i] = pad(rander.Intn(*nimg))
	}

	data := struct {
		Title   string
		Links   []string
		Images  []string
		Content []string
	}{pad(u), links, imgs, genText(u)}
	if err := t.Execute(w, data); err != nil {
		log.Fatal(err)
	}
	// log.Printf("%s %s", r.URL.Path, time.Since(now))
	s.stat.inc("hit")
}

func (s *server) image(w http.ResponseWriter, r *http.Request) {
	u, err := strconv.Atoi(r.URL.Path[5:]) // 5 = len("/img/")
	if err != nil {
		http.NotFound(w, r)
		s.stat.inc("bad")
		return
	}
	if u > *nimg || u < 0 {
		http.NotFound(w, r)
		s.stat.inc("bad")
		return
	}
	bits := s.imgCache[u]
	if bits == nil {
		log.Println("Generating image", u)
		bits = genImage(u)
		s.imgCache[u] = bits
	}
	w.Write(bits)
	s.stat.inc("img")
}

func (s *server) redirect(w http.ResponseWriter, r *http.Request) {
	log.Println("Redirecting", r.URL.String())
	http.Redirect(w, r, "/page/"+pad(rand.Intn(s.g.Size())), http.StatusTemporaryRedirect)
	s.stat.inc("mis")
}

func main() {
	flag.Parse()
	mask = fmt.Sprintf("%%0%dd", int(math.Ceil(math.Log10(float64(*size)))))
	runtime.GOMAXPROCS(runtime.NumCPU())
	s := &server{
		g:        genGraph(*size, *links),
		imgCache: make([][]byte, *nimg),
		stat:     newStats(),
	}
	http.HandleFunc("/favicon.ico", http.NotFound)
	http.HandleFunc("/page/", s.page)
	http.HandleFunc("/img/", s.image)
	http.HandleFunc("/", s.redirect)
	http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("PONG")) })
	go s.logger()
	http.ListenAndServe(":"+strconv.Itoa(*port), nil)
}
