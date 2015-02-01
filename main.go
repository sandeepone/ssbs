package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gorilla/pat"
)

func main() {
	bindAddr := ":5252"

	flag.StringVar(&bindAddr, "bind", bindAddr, "bind address, e.g. :5252")
	flag.Parse()

	log.Printf("Listening on %s", bindAddr)

	p := pat.New()
	p.Path("/healthcheck").Methods("GET").HandlerFunc(healthcheck)
	p.Path("/build").Methods("POST").HandlerFunc(build)

	err := http.ListenAndServe(bindAddr, p)
	if err != nil {
		log.Printf("Error binding to %s: %v", bindAddr, err)
	}
}

func healthcheck(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(200)
}

type buildInput struct {
	// The GitHub repo to build (e.g. ian-kent/ssbs)
	Repo string `json:"repo"`
	// The commit to build (e.g. 1ba8d6s or master)
	Commit string `json:"commit"`
	// A pattern to find artifacts (e.g. ssbs-*.zip)
	Output string `json:"output"`
	// A list of steps to execute (e.g. [ ["make"], ["make", "dist"] ])
	Steps [][]string `json:"steps"`
}
type buildResponse struct {
	// The result of each step
	// Only includes user-specified steps, unless a built-in step
	// fails (e.g. git clone)
	Steps []buildOutput `json:"steps"`
	// All artifacts matching the pattern (base64 encoded)
	Artifacts map[string]string `json:"artifacts"`
}
type buildOutput struct {
	// The step executed
	Step []string `json:"command"`
	// Any error returned
	Error error `json:"error,omitempty"`
	// STDOUT from the step process
	Stdout string `json:"stdout"`
	// STDERR from the step process
	Stderr string `json:"stderr"`
}

func runCommand(workDir, cmd string, args ...string) (string, string, error) {
	c := exec.Command(cmd, args...)
	var out bytes.Buffer
	var err bytes.Buffer
	c.Stdout = &out
	c.Stderr = &err
	c.Dir = workDir

	cErr := c.Run()
	if cErr != nil {
		log.Printf("Error executing command: %s %s\nSTDOUT:\n%s\nSTDERR:%s", cmd, args, out.String(), err.String())
		return out.String(), err.String(), cErr
	}

	log.Printf("Successfully executed command: %s %s\nSTDOUT:\n%s\nSTDERR:\n%s", cmd, args, out.String(), err.String())
	return out.String(), err.String(), nil
}

func build(w http.ResponseWriter, req *http.Request) {
	b, err := ioutil.ReadAll(req.Body)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	req.Body.Close()

	var i buildInput
	err = json.Unmarshal(b, &i)
	if err != nil {
		w.WriteHeader(400)
		return
	}

	ts := fmt.Sprintf("%d", time.Now().Unix())
	dRn := strings.Replace(i.Repo, "/", "+", -1)
	workParent := "./workdir/" + dRn + "-" + ts
	workDir := workParent + "/" + i.Repo

	defer cleanup(workParent)

	r := &buildResponse{
		Steps: make([]buildOutput, 0),
	}
	var o buildOutput

	o.Stdout, o.Stderr, o.Error = runCommand(".", "git", "clone", "git@github.com:"+i.Repo+".git", workDir)
	if o.Error != nil {
		log.Printf("Error cloning repo %s: %s", i.Repo, o.Error)
		r.Steps = append(r.Steps, o)
		b, err := json.Marshal(&r)
		if err != nil {
			w.WriteHeader(500)
			w.Write([]byte("Failed to marshal output"))
			return
		}
		w.WriteHeader(200)
		w.Write(b)
		return
	}
	log.Printf("Cloned repo %s", i.Repo)

	o.Stdout, o.Stderr, o.Error = runCommand(workDir, "git", "checkout", i.Commit)
	if o.Error != nil {
		log.Printf("Error checking out commit %s: %s", i.Commit, o.Error)
		b, err := json.Marshal(&r)
		if err != nil {
			w.WriteHeader(500)
			w.Write([]byte("Failed to marshal output"))
			return
		}
		w.WriteHeader(200)
		w.Write(b)
		return
	}
	log.Printf("Checked out %s\n", i.Commit)

	for _, step := range i.Steps {
		var o1 buildOutput
		o1.Step = step
		o1.Stdout, o1.Stderr, o1.Error = runCommand(workDir, step[0], step[1:]...)
		r.Steps = append(r.Steps, o1)
		if o1.Error != nil {
			log.Printf("Error running step %s for %s: %s", step, i.Repo, o1.Error)
			b, err := json.Marshal(&r)
			if err != nil {
				w.WriteHeader(500)
				w.Write([]byte("Failed to marshal output"))
				return
			}
			w.WriteHeader(200)
			w.Write(b)
			return
		}
		log.Printf("Step completed for %s", i.Repo)
	}

	if len(i.Output) > 0 {
		o.Stdout, o.Stderr, o.Error = runCommand(workDir, "find", ".", "-name", i.Output)
		if o.Error != nil {
			log.Printf("Error finding artifacts: %s", o.Error)
			r.Steps = append(r.Steps, o)
			b, err := json.Marshal(&r)
			if err != nil {
				w.WriteHeader(500)
				w.Write([]byte("Failed to marshal output"))
				return
			}
			w.WriteHeader(200)
			w.Write(b)
			return
		}
		log.Printf("Found artifacts: %s\n", o.Stdout)

		r.Artifacts = make(map[string]string)

		arts := strings.Split(o.Stdout, "\n")
		for _, art := range arts {
			art = strings.TrimSpace(art)
			if len(art) > 0 {
				b, err := ioutil.ReadFile(workDir + "/" + art)
				if err != nil {
					r.Artifacts[art] = fmt.Sprintf("Error reading artifact: %s", err)
					log.Printf("Failed to add artifact %s: %s", art, err)
					continue
				}
				r.Artifacts[art] = base64.StdEncoding.EncodeToString(b)
				log.Printf("Added artifact %s (%d bytes)", art, len(b))
			}
		}
	}

	b, err = json.Marshal(&r)
	if err != nil {
		w.WriteHeader(500)
		w.Write([]byte("Failed to marshal output"))
		return
	}
	w.WriteHeader(200)
	w.Write(b)
	return
}

func cleanup(workParent string) {
	err := os.RemoveAll(workParent)
	if err != nil {
		log.Printf("WARNING: Failed to remove workParent: %s", err)
	}
}
