package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/karrick/godirwalk"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	portHelp  = "TCP port blackd listens on."
	diffHelp  = "Don't write the files back, just output a diff for each file on stdout."
	checkHelp = "Don't write the files back, just return the status. " +
		"Return code 0 means nothing would change. Return code 1 means some files would be reformatted. " +
		"Return code 123 means there was an internal error."
)

var (
	ports = kingpin.Flag("port", portHelp).Required().Uint16List()
	diff  = kingpin.Flag("diff", diffHelp).Bool()
	check = kingpin.Flag("check", checkHelp).Bool()
	files = kingpin.Arg("files", "Files to format").Strings()
)

type Action int

const (
	Unchanged Action = iota
	Reformatted
	WouldBeReformatted
	Error
)

func infof(format string, v ...interface{}) {
	_, _ = fmt.Fprintf(os.Stderr, format, v...)
}

func main() {
	log.SetFlags(0)
	kingpin.Parse()

	pathQueue := make(chan string, len(*ports))
	actQueue := make(chan Action, 99)

	wg := sync.WaitGroup{}
	for _, port := range *ports {
		wg.Add(1)
		go func(port string) {
			conf := BlackConfig{
				Url:   fmt.Sprintf("http://127.0.0.1:%s", port),
				Check: *check,
				Diff:  *diff,
			}
			for path := range pathQueue {
				actQueue <- processPath(conf, path)
			}
			wg.Done()
		}(strconv.FormatUint(uint64(port), 10))
	}

	go func() {
		wg.Wait()
		close(actQueue)
	}()

	go walkDirectories(*files, pathQueue)

	os.Exit(report(*check, actQueue))
}

func walkDirectories(paths []string, pathQueue chan<- string) {
	for _, path := range paths {
		err := godirwalk.Walk(path, &godirwalk.Options{
			FollowSymbolicLinks: true,
			Unsorted:            true,
			AllowNonDirectory:   true,
			Callback: func(path string, de *godirwalk.Dirent) error {
				if de.IsRegular() && strings.HasSuffix(path, ".py") {
					pathQueue <- path
				}
				return nil
			},
			ErrorCallback: func(path string, err error) godirwalk.ErrorAction {
				infof("cannot format %s: %v\n", path, err)
				return godirwalk.SkipNode
			},
		})
		if err != nil {
			log.Fatalf("error traversing %s: %v", path, err)
		}
	}
	close(pathQueue)
}

func report(check bool, actQueue <-chan Action) int {
	exitCode := 0
	unchangedCount := 0
	reformattedCount := 0
	errorCount := 0
	for act := range actQueue {
		switch act {
		case Unchanged:
			unchangedCount += 1
		case Reformatted:
			reformattedCount += 1
		case WouldBeReformatted:
			reformattedCount += 1
			if exitCode < 1 {
				exitCode = 1
			}
		case Error:
			errorCount += 1
			exitCode = 123
		}
	}

	if unchangedCount == 0 && reformattedCount == 0 && errorCount == 0 {
		fmt.Println("No Python files are present to be formatted. Nothing to do 😴")
		return exitCode
	}

	b := strings.Builder{}
	if reformattedCount > 0 {
		reportCount(&b, check, reformattedCount, "would be reformatted", "reformatted")
	}
	if unchangedCount > 0 {
		reportCount(&b, check, unchangedCount, "would be left unchanged", "left unchanged")
	}
	if errorCount > 0 {
		reportCount(&b, check, errorCount, "would fail to reformat", "failed to reformat")
	}
	b.WriteRune('.')
	log.Println(b.String())
	return exitCode
}

func reportCount(buf *strings.Builder, check bool, count int, statusWithCheck, statusWithoutCheck string) {
	buf.Grow(10 + len(statusWithCheck))
	if buf.Len() > 0 {
		buf.WriteString(", ")
	}
	buf.WriteString(strconv.Itoa(count))
	buf.WriteString(" file")
	if count > 1 {
		buf.WriteString("s ")
	} else {
		buf.WriteRune(' ')
	}
	if check {
		buf.WriteString(statusWithCheck)
	} else {
		buf.WriteString(statusWithoutCheck)
	}
}

func processPath(conf BlackConfig, path string) Action {
	resp, err := queryBlackd(conf, path)
	if err != nil {
		infof("error: cannot format %s: %v\n", path, err)
		return Error
	}
	defer resp.Body.Close()
	defer io.Copy(ioutil.Discard, resp.Body)

	res, blackErr := newBlackResult(resp)
	if blackErr != nil {
		if blackErr.Syntax {
			infof("%s: %s\n", path, blackErr.Msg)
		} else {
			infof("cannot format %s: %s\n", path, blackErr.Msg)
		}
		return Error
	}
	if !res.Changed {
		return Unchanged
	}
	if conf.Diff && !printDiff(path, res.Text) {
		return Error
	}
	if conf.Check {
		infof("would reformat %s\n", path)
		return WouldBeReformatted
	}
	if err = overwritePath(path, res.Text); err != nil {
		log.Print(err)
		return Error
	}
	infof("reformatted %s\n", path)
	return Reformatted
}

func printDiff(path string, diff io.Reader) bool {
	buf := bufio.NewReader(diff)
	ok := printDiffHeader(path, "In", buf)
	ok = ok && printDiffHeader(path, "Out", buf)
	if !ok {
		infof("%s: internal error: blackd returned an invalid diff\n", path)
		return false
	}
	_, err := io.Copy(os.Stdout, buf)
	if err != nil {
		log.Print(err)
		return false
	}
	return true
}

func printDiffHeader(path string, oldPath string, buf *bufio.Reader) bool {
	header, err := buf.ReadString('\n')
	if err != nil {
		return false
	}
	header = strings.Replace(header, oldPath, path, 1)
	_, _ = os.Stdout.WriteString(header)
	return true
}

func newHttpClient() *http.Client {
	// http://tleyden.github.io/blog/2016/11/21/tuning-the-go-http-client-library-for-load-testing/
	// Customize the Transport to have a larger connection pool.
	defaultRoundTripper := http.DefaultTransport
	defaultTransportPointer, ok := defaultRoundTripper.(*http.Transport)
	if !ok {
		panic(fmt.Sprintf("defaultRoundTripper is not an *http.Transport"))
	}
	// Dereference to get a copy of the struct that the pointer points to.
	defaultTransport := *defaultTransportPointer
	defaultTransport.MaxIdleConns = 100
	defaultTransport.MaxIdleConnsPerHost = 100

	return &http.Client{
		Transport: &defaultTransport,
		Timeout:   5 * time.Second,
	}
}

var (
	client = newHttpClient()
)

func queryBlackd(conf BlackConfig, path string) (*http.Response, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	r := bufio.NewReader(file)
	req, err := http.NewRequest("POST", conf.Url, r)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}

	if conf.Diff {
		req.Header.Set("X-Diff", "1")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: couldn't reach blackd: %v", path, err)
	}
	return resp, nil
}

func newBlackResult(resp *http.Response) (*BlackResult, *BlackError) {
	switch resp.StatusCode {
	case 204:
		return &BlackResult{}, nil
	case 200:
		return &BlackResult{Changed: true, Text: resp.Body}, nil
	case 400:
		return nil, &BlackError{Syntax: true, Msg: newStringFromReader(resp.Body)}
	case 500:
		return nil, &BlackError{Msg: newStringFromReader(resp.Body)}
	}
	log.Fatalf("unsupported HTTP status: %v", resp.StatusCode)
	return nil, nil // never reached
}

func overwritePath(path string, newContents io.Reader) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("%s: cannot format: %v", path, err)
	}
	defer file.Close()
	w := bufio.NewWriter(file)
	_, err = io.Copy(w, newContents)
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		return fmt.Errorf("%s: formatting failed: %v", path, err)
	}
	return nil
}

type BlackConfig struct {
	Url   string
	Diff  bool
	Check bool
}

type BlackResult struct {
	Changed bool
	Text    io.Reader
}

type BlackError struct {
	Syntax bool
	Msg    string
}

func newStringFromReader(r io.Reader) string {
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r)
	if err != nil {
		panic(err)
	}
	return buf.String()
}
