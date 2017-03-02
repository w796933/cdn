package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	redis "gopkg.in/redis.v5"

	"github.com/bign8/cdn/health"
)

const cdnHeader = "x-bign8-cdn"

// Variables to identify the build
var (
	Version = "Unknown"

	target = flag.String("target", os.Getenv("TARGET"), "target hostname")
	port   = flag.Int("port", 8081, "What port to run server on")
	cap    = flag.Int("cap", 10, "How many requests to store in cache")
)

func check(err error) {
	if err != nil {
		panic(err)
	}
}

type response struct {
	code int
	head http.Header
	body []byte
}

func newResponse(res *http.Response) (r response, err error) {
	r = response{
		code: res.StatusCode,
		head: res.Header,
	}
	r.body, err = ioutil.ReadAll(res.Body)
	if err == nil {
		res.Body = ioutil.NopCloser(bytes.NewReader(r.body))
	}
	return r, err
}

func (r *response) Send(w http.ResponseWriter) {
	for key, value := range r.head {
		w.Header()[key] = value
	}
	w.WriteHeader(r.code)
	w.Write(r.body)
}

type cdn struct {
	me  string
	rp  *httputil.ReverseProxy
	red *redis.Client
	cap int

	cache map[string]response
	mu    sync.RWMutex

	ring   []string
	ringMu sync.RWMutex
}

func (c *cdn) RoundTrip(req *http.Request) (*http.Response, error) {
	log.Println("Proxying!", req.URL.String())
	res, err := http.DefaultTransport.RoundTrip(req)
	if err == nil && res.StatusCode == http.StatusOK { // TODO: trap other headers and respect cache codes
		var r response
		r, err = newResponse(res)
		if err != nil {
			return res, err
		}
		c.mu.Lock()
		c.cache[req.URL.Path] = r
		c.mu.Unlock()
	}
	return res, err
}

func (c *cdn) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	c.mu.RLock()
	item, ok := c.cache[req.URL.Path] // TODO: respect cache timeouts
	c.mu.RUnlock()

	log.Printf("%s got header %v %v", c.me, req.Header.Get(cdnHeader), req.UserAgent())

	if ok {
		item.Send(w)
	} else if req.Header.Get(cdnHeader) != "" || req.Method == http.MethodConnect || req.UserAgent() == cdnHeader {
		log.Print(c.me, " couldn't find response for neighbor")
		http.NotFound(w, req)
	} else if item, ok = c.checkNeighbors(req.URL.Path); ok {
		item.Send(w)
	} else {
		c.rp.ServeHTTP(w, req)
	}
}

type neighborResult struct {
	res response
	err error
}

func (c *cdn) checkNeighbors(path string) (result response, found bool) {
	log.Print("TODO: Proxy request to neighbors!")
	c.ringMu.RLock()
	neighbors := c.ring[:]
	c.ringMu.RUnlock()
	ctx, done := context.WithTimeout(context.Background(), time.Second*5)

	// Parallel fetching function
	fetch := func(n string, fin chan<- neighborResult) {
		target := "http://" + n + ":" + strconv.Itoa(*port) + "/" + path
		var r neighborResult
		if req, err := http.NewRequest(http.MethodConnect, target, nil); err != nil {
			r.err = err
		} else {
			req = req.WithContext(ctx)
			req.Header.Set("User-Agent", cdnHeader)
			req.Header.Set(cdnHeader, c.me)
			if res, err := http.DefaultClient.Do(req); err != nil {
				r.err = err
			} else {
				r.res, r.err = newResponse(res)
			}
		}
		fin <- r
	}

	// Fetch requests in paralell
	results := make(chan neighborResult, len(neighbors))
	for _, neighbor := range neighbors {
		go fetch(neighbor, results)
	}

	// fetch all results until found
	for i := 0; i < len(neighbors); i++ {
		back := <-results
		if !found && back.err == nil && back.res.code == http.StatusOK {
			log.Print(c.me, "Found response on neighbor")
			done()
			found = true
			result = back.res
		} else if back.err != nil {
			log.Print(c.me, "Problem fetching from neighbour")
		}
	}
	done()
	return result, found
}

func (c *cdn) monitorNeighbors() {
	var last string
	for {
		// Get set from redis
		servers, err := c.red.SMembers("cdn-servers").Result()
		if err != nil {
			log.Print("Cannot fetch neighbor set:", err)
			continue
		}

		// Generate usable list for consumers
		result := make([]string, 0, len(servers)-1)
		for _, server := range servers {
			if server != c.me {
				result = append(result, server)
			}
		}
		sort.Strings(result)

		// Use string representation of neighbors to determine if update is necessary
		if next := strings.Join(result, ", "); next != last {
			log.Print(c.me, "is updating server list: ["+next+"]")
			last = next
			c.ringMu.Lock()
			c.ring = result
			c.ringMu.Unlock()
		} else {
			// log.Print(c.me, "is found no update for server list")
		}

		// Wait for another cycle // TODO: listen to pub-sub for updates or something
		time.Sleep(time.Second * 5)
	}
}

func main() {
	health.Check()
	uri, err := url.Parse(*target)
	check(err)

	host, err := os.Hostname()
	check(err)

	red := redis.NewClient(&redis.Options{Addr: "redis:6379"}) // TODO: swap back to `redis` for compose
	check(red.Ping().Err())
	red.SAdd("cdn-servers", host)

	cdnHandler := &cdn{
		me:    host,
		rp:    httputil.NewSingleHostReverseProxy(uri),
		cap:   *cap,
		red:   red,
		cache: make(map[string]response),
	}
	cdnHandler.rp.Transport = cdnHandler

	http.Handle("/", cdnHandler)
	http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("PONG")) })
	http.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		if hdr := r.Header.Get("X-BIGN8-CDN"); hdr != "" {
			log.Printf("%s: getting version request from: %v", host, hdr)
		}
		w.Write([]byte(Version))
	})
	http.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		if err := red.Incr("counter").Err(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		n, err := red.Get("counter").Int64()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte(fmt.Sprintf("Hey! hit %d times", n)))
	})
	http.HandleFunc("/other", func(w http.ResponseWriter, r *http.Request) {
		parts, err := red.SMembers("cdn-servers").Result()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		neighbor := parts[rand.Intn(len(parts))] // TODO bign8: make sure this isn't me
		req, _ := http.NewRequest("GET", "http://"+neighbor+":8081/version", nil)
		req.Header.Set("X-BIGN8-CDN", host)
		res, err := http.DefaultClient.Do(req.WithContext(context.TODO()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusExpectationFailed)
			return
		}
		w.WriteHeader(res.StatusCode)
		io.Copy(w, res.Body)
		res.Body.Close()
		w.Write([]byte(fmt.Sprintf("\nMe:%s\nOther:%s", host, neighbor)))
	})

	// Actually start the server
	log.Printf("ReverseProxy for %q serving on :%d\n", *target, *port)
	go cdnHandler.monitorNeighbors()
	check(http.ListenAndServe(":"+strconv.Itoa(*port), nil))
}
