package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/karrick/godirwalk"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	portHelp  = "TCP port blackd listens on."
	diffHelp  = "Don't write the files back, just output a diff for each file on stdout."
	checkHelp = "Don't write the files back, just return the status. " +
		"Return code 0 means nothing would change. Return code 1 means some files would be reformatted. " +
		"Return code 123 means there was an internal error."
	maxConnDefault = "99"
	maxConnHelp    = "Maximum number of simultaneous connections (goroutines) to Blackd. Defaults to " + maxConnDefault
)

var (
	port    = kingpin.Flag("port", portHelp).Required().Envar("BLACKD_PORT").Uint16()
	diff    = kingpin.Flag("diff", diffHelp).Bool()
	check   = kingpin.Flag("check", checkHelp).Bool()
	maxConn = kingpin.Flag("max-connections", maxConnHelp).Default(maxConnDefault).Envar("BLACKD_MAX_CONNECTIONS").Uint16()
	files   = kingpin.Arg("files", "Files to format").Strings()
)

type Action int

const (
	Unchanged Action = iota
	Reformatted
	WouldBeReformatted
	Error
)

var EOL = []byte("\n")

func infof(format string, v ...interface{}) {
	_, _ = fmt.Fprintf(os.Stderr, format, v...)
	_, _ = os.Stderr.Write(EOL)
}

func main() {
	log.SetFlags(0)
	kingpin.Parse()
	conf := BlackConfig{
		Url:   fmt.Sprintf("http://127.0.0.1:%s", strconv.FormatUint(uint64(*port), 10)),
		Check: *check,
		Diff:  *diff,
	}
	pathQueue := make(chan string, *maxConn+1)
	actQueue := make(chan Action)
	exitQueue := make(chan int)

	wg := sync.WaitGroup{}
	for i := 0; i < int(*maxConn); i++ {
		wg.Add(1)
		go func(i int) {
			for path := range pathQueue {
				actQueue <- processPath(conf, path)
			}
			wg.Done()
		}(i)
	}

	go func() {
		wg.Wait()
		close(actQueue)
	}()

	go report(conf.Check, actQueue, exitQueue)

	for _, path := range *files {
		err := godirwalk.Walk(path, &godirwalk.Options{
			FollowSymbolicLinks: true,
			Unsorted:            true,
			AllowNonDirectory:   true,
			Callback: func(path string, de *godirwalk.Dirent) error {
				if (de.IsRegular() || de.IsSymlink()) && strings.HasSuffix(path, ".py") {
					pathQueue <- path
				}
				return nil
			},
			ErrorCallback: func(path string, err error) godirwalk.ErrorAction {
				infof("cannot format %s: %v", path, err)
				return godirwalk.SkipNode
			},
		})
		if err != nil {
			log.Fatalf("error traversing %s: %v", path, err)
		}
	}
	close(pathQueue)
	os.Exit(<-exitQueue)
}

func report(check bool, actQueue <-chan Action, exitQueue chan<- int) {
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
		exitQueue <- exitCode
		return
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
	exitQueue <- exitCode
	return
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
		infof("error: cannot format %s: %v", path, err)
		return Error
	}
	defer resp.Body.Close()

	res, blackErr := newBlackResult(resp)
	if blackErr != nil {
		if blackErr.Syntax {
			infof("%s: %s", path, blackErr.Msg)
		} else {
			infof("cannot format %s: %s", path, blackErr.Msg)
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
		infof("would reformat %s", path)
		return WouldBeReformatted
	}
	if err = overwritePath(path, res.Text); err != nil {
		log.Print(err)
		return Error
	}
	infof("reformatted %s", path)
	return Reformatted
}

func printDiff(path string, diff io.Reader) bool {
	buf := bufio.NewReader(diff)
	ok := printDiffHeader(path, "In", buf)
	ok = ok && printDiffHeader(path, "Out", buf)
	if !ok {
		infof("%s: internal error: blackd returned an invalid diff", path)
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

var client = &http.Client{}

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
