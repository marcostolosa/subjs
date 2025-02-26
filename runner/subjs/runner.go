package subjs

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const version = `1.0.2`

type SubJS struct {
	client *http.Client
	opts   *Options
}

func New(opts *Options) *SubJS {
	c := &http.Client{
		Timeout:   time.Duration(opts.Timeout) * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	return &SubJS{client: c, opts: opts}
}

func (s *SubJS) Run() error {
	// setup input
	var input *os.File
	var err error
	// if input file not specified then read from stdin
	if s.opts.InputFile == "" {
		input = os.Stdin
	} else {
		// otherwise read from file
		input, err = os.Open(s.opts.InputFile)
		if err != nil {
			return fmt.Errorf("Could not open input file: %s", err)
		}
		defer input.Close()
	}

	// init channels
	urls := make(chan string)
	results := make(chan string)

	// start workers
	var w sync.WaitGroup
	for i := 0; i < s.opts.Workers; i++ {
		w.Add(1)
		go func() {
			s.fetch(urls, results)
			w.Done()
		}()
	}
	// setup output
	var out sync.WaitGroup
	out.Add(1)
	go func() {
		for result := range results {
			fmt.Println(result)
		}
		out.Done()
	}()
	scan := bufio.NewScanner(input)
	for scan.Scan() {
		u := scan.Text()
		if u != "" {
			urls <- u
		}
	}
	close(urls)
	w.Wait()
	close(results)
	out.Wait()
	return nil
}

func (s *SubJS) fetch(urls <-chan string, results chan string) {
	// Create a set to track processed URLs
	processedURLs := make(map[string]bool)

	for u := range urls {
		if processedURLs[u] {
			continue // Skip already processed URLs
		}
		processedURLs[u] = true

		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			continue
		}
		if s.opts.UserAgent != "" {
			req.Header.Add("User-Agent", s.opts.UserAgent)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			continue
		}

		// Read the complete response
		body, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}

		// Try to parse as HTML
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
		if err != nil {
			continue
		}

		parsedURL, err := url.Parse(u)
		if err != nil {
			continue
		}

		// Process script tags - using scriptTag instead of s to avoid shadowing
		doc.Find("script").Each(func(index int, scriptTag *goquery.Selection) {
			js, exists := scriptTag.Attr("src")
			if exists && js != "" {
				// Resolve the URL
				resolvedJS := resolveScriptURL(parsedURL, js)

				// Report the script
				if !processedURLs[resolvedJS] {
					results <- resolvedJS
					processedURLs[resolvedJS] = true

					// Check if this looks like a webpack bundle
					if isWebpackBundle(resolvedJS) {
						// Fetch the webpack script
						webpackReq, err := http.NewRequest("GET", resolvedJS, nil)
						if err != nil {
							return
						}
						if s.opts.UserAgent != "" {
							webpackReq.Header.Add("User-Agent", s.opts.UserAgent)
						}
						webpackResp, err := s.client.Do(webpackReq)
						if err != nil {
							return
						}

						webpackBody, err := ioutil.ReadAll(webpackResp.Body)
						webpackResp.Body.Close()
						if err != nil {
							return
						}

						// Process the webpack file to extract chunk references
						s.ProcessWebpackFile(resolvedJS, string(webpackBody), results)
					}
				}
			}

			// Find JS references in script tag content
			r := regexp.MustCompile(`[(\w./:)]*js`)
			matches := r.FindAllString(scriptTag.Contents().Text(), -1)
			for _, js := range matches {
				if strings.HasPrefix(js, "//") {
					js := fmt.Sprintf("%s:%s", parsedURL.Scheme, js)
					if !processedURLs[js] {
						results <- js
						processedURLs[js] = true
					}
				} else if strings.HasPrefix(js, "/") {
					js := fmt.Sprintf("%s://%s%s", parsedURL.Scheme, parsedURL.Host, js)
					if !processedURLs[js] {
						results <- js
						processedURLs[js] = true
					}
				}
			}
		})

		// Process div tags with data-script-src attribute - using divTag instead of s
		doc.Find("div").Each(func(index int, divTag *goquery.Selection) {
			js, exists := divTag.Attr("data-script-src")
			if exists && js != "" {
				resolvedJS := resolveScriptURL(parsedURL, js)
				if !processedURLs[resolvedJS] {
					results <- resolvedJS
					processedURLs[resolvedJS] = true
				}
			}
		})
	}
}

// ProcessWebpackFile extracts all JavaScript chunk paths from a webpack bundle
func (s *SubJS) ProcessWebpackFile(webpackURL string, content string, results chan string) {
	baseURL, err := url.Parse(webpackURL)
	if err != nil {
		return
	}

	// Track processed URLs to avoid duplicates
	processedPaths := make(map[string]bool)

	// Ensure path has _next/ prefix if not already present
	ensureNextPrefix := func(path string) string {
		if !strings.HasPrefix(path, "/_next/") && !strings.HasPrefix(path, "_next/") {
			if strings.HasPrefix(path, "/") {
				return "/_next" + path
			}
			return "/_next/" + path
		}
		return path
	}

	// Pattern 1: Extract direct chunk references
	// Example: a.u=e=>2986===e?"static/chunks/2986-2488e3e4a13aed5b.js"
	directChunkPattern := regexp.MustCompile(`(\d+)===e\?"([^"]+)"`)
	for _, match := range directChunkPattern.FindAllStringSubmatch(content, -1) {
		chunkPath := ensureNextPrefix(match[2])
		resolvedURL := resolveScriptURL(baseURL, chunkPath)

		if !processedPaths[resolvedURL] {
			results <- resolvedURL
			processedPaths[resolvedURL] = true
		}
	}

	// Pattern 2: Complex mapping using two dictionaries
	// Example: "static/chunks/"+(({1027:"4b26d002",...})[e]||e)+"."+({142:"b1a9bae1a2949d82",...})[e]+".js"
	complexPattern := regexp.MustCompile(`"(static/chunks/)"\+\(\({([^}]+)}\)\[e\]\|\|e\)\+"\."\+\({([^}]+)}\)\[e\]\+"\.js"`)
	complexMatches := complexPattern.FindStringSubmatch(content)

	if len(complexMatches) > 3 {
		basePath := complexMatches[1]
		idMapStr := complexMatches[2]
		hashMapStr := complexMatches[3]

		// Parse ID map (maps IDs to prefixes)
		idMap := parseJSMap(idMapStr)

		// Parse hash map (maps IDs to hashes)
		hashMap := parseJSMap(hashMapStr)

		// Generate chunk URLs for each hash entry
		for id, hash := range hashMap {
			var chunkName string
			if namedID, ok := idMap[id]; ok {
				chunkName = namedID
			} else {
				chunkName = id
			}

			chunkPath := basePath + chunkName + "." + hash + ".js"
			chunkPath = ensureNextPrefix(chunkPath)
			resolvedURL := resolveScriptURL(baseURL, chunkPath)

			if !processedPaths[resolvedURL] {
				results <- resolvedURL
				processedPaths[resolvedURL] = true
			}
		}
	}

	// Pattern 3: Look for a.p (public path) + a.u (chunk URL) patterns
	// Example: a.p+"static/chunks/pages/about-12345.js"
	publicPathPattern := regexp.MustCompile(`a\.p\+"([^"]+\.js)"`)
	for _, match := range publicPathPattern.FindAllStringSubmatch(content, -1) {
		chunkPath := match[1]
		// For this pattern, we use _next/ directly since it's already handled in resolveScriptURL
		resolvedURL := resolveScriptURL(baseURL, ensureNextPrefix(chunkPath))

		if !processedPaths[resolvedURL] {
			results <- resolvedURL
			processedPaths[resolvedURL] = true
		}
	}

	// Pattern 4: Match a.u function that maps IDs to file paths
	// Example: a.u=e=>2986===e?"static/chunks/2986-2488e3e4a13aed5b.js":7699===e?"static/chunks/...
	auFunctionPattern := regexp.MustCompile(`a\.u=e=>([^}]+)`)
	auMatches := auFunctionPattern.FindStringSubmatch(content)
	if len(auMatches) > 1 {
		auContent := auMatches[1]

		// Extract each condition and path
		chunkPattern := regexp.MustCompile(`(\d+)===e\?"([^"]+)"`)
		for _, match := range chunkPattern.FindAllStringSubmatch(auContent, -1) {
			chunkPath := ensureNextPrefix(match[2])
			resolvedURL := resolveScriptURL(baseURL, chunkPath)

			if !processedPaths[resolvedURL] {
				results <- resolvedURL
				processedPaths[resolvedURL] = true
			}
		}
	}
}

// parseJSMap extracts key-value pairs from JavaScript object literal strings
func parseJSMap(jsMapStr string) map[string]string {
	result := make(map[string]string)

	// Find all key-value pairs with regex
	// Format: 142:"b1a9bae1a2949d82"
	pairPattern := regexp.MustCompile(`(\d+):"([^"]+)"`)
	for _, match := range pairPattern.FindAllStringSubmatch(jsMapStr, -1) {
		key := match[1]
		value := match[2]
		result[key] = value
	}

	return result
}

// resolveScriptURL resolves a script path relative to a base URL
func resolveScriptURL(baseURL *url.URL, path string) string {
	// Already absolute URL
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}

	// Protocol-relative URL
	if strings.HasPrefix(path, "//") {
		return baseURL.Scheme + ":" + path
	}

	// NextJS-specific path handling
	if strings.HasPrefix(path, "/_next/") || strings.HasPrefix(path, "_next/") {
		// Ensure consistent format with leading slash
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}

		return fmt.Sprintf("%s://%s%s", baseURL.Scheme, baseURL.Host, path)
	}

	// Ensure path starts with slash for joining
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// Create a new URL by joining base with path
	resolved := &url.URL{
		Scheme: baseURL.Scheme,
		Host:   baseURL.Host,
		Path:   path,
	}

	return resolved.String()
}

// isWebpackBundle determines if a URL appears to be a webpack bundle
func isWebpackBundle(url string) bool {
	return strings.Contains(url, "webpack") ||
		strings.Contains(url, "bundle") ||
		strings.Contains(url, "chunks") ||
		strings.Contains(url, "_next/static")
}
