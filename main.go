package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type httpLogger struct {
	next http.RoundTripper
}

var _ http.RoundTripper = httpLogger{}

func (hl httpLogger) RoundTrip(request *http.Request) (*http.Response, error) {
	fmt.Printf("%s %s\n", request.Method, request.URL.String())
	return hl.next.RoundTrip(request)
}

type closableReader struct {
	r io.Reader
}

var _ io.ReadCloser = closableReader{}

func (cr closableReader) Read(p []byte) (int, error) {
	return cr.r.Read(p)
}

func (closableReader) Close() error {
	return nil
}

type badHttpStatus struct {
	code int
}

var _ error = badHttpStatus{}

func (bhs badHttpStatus) Error() string {
	return fmt.Sprintf("HTTP %d", bhs.code)
}

func main() {
	host := flag.String("host", "", "HOST")
	port := flag.String("port", "5665", "PORT")
	ca := flag.String("ca", "", "FILE")
	cn := flag.String("cn", "", "COMMON_NAME")
	user := flag.String("user", "", "USERNAME")

	flag.Parse()

	if *host == "" {
		fmt.Fprintln(os.Stderr, "-host missing")
		os.Exit(2)
	}

	if *port == "" {
		fmt.Fprintln(os.Stderr, "-port missing")
		os.Exit(2)
	}

	if *ca == "" {
		fmt.Fprintln(os.Stderr, "-ca missing")
		os.Exit(2)
	}

	if *cn == "" {
		fmt.Fprintln(os.Stderr, "-cn missing")
		os.Exit(2)
	}

	if *user == "" {
		fmt.Fprintln(os.Stderr, "-user missing")
		os.Exit(2)
	}

	pass := os.Getenv("I2_PASS")
	if pass == "" {
		fmt.Fprintln(os.Stderr, "$I2_PASS missing")
		os.Exit(2)
	}

	cas := x509.NewCertPool()

	{
		pem, errRF := ioutil.ReadFile(*ca)
		if errRF != nil {
			fmt.Fprintln(os.Stderr, errRF.Error())
			os.Exit(1)
		}

		if !cas.AppendCertsFromPEM(pem) {
			fmt.Fprintln(os.Stderr, "bad CA cert")
			os.Exit(1)
		}
	}

	client := &http.Client{Transport: httpLogger{&http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: cas, ServerName: *cn},
	}}}

	req := &http.Request{
		URL:    &url.URL{Scheme: "https", Host: *host + ":" + *port},
		Header: http.Header{},
		//Header: http.Header{"Accept": []string{"application/json"}},
	}

	req.SetBasicAuth(*user, pass)

	var packages struct {
		Results []struct {
			ActiveStage string `json:"active-stage"`
			Name        string `json:"name"`
		} `json:"results"`
	}

	if errSR := sendReq(client, req, "GET", "/v1/config/packages", nil, &packages); errSR != nil {
		fmt.Fprintln(os.Stderr, errSR.Error())
		os.Exit(1)
	}

	for _, pkg := range packages.Results {
		if pkg.Name != "" && pkg.ActiveStage != "" /*&& !strings.HasPrefix(pkg.Name, "_")*/ {
			var files struct {
				Results []struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"results"`
			}

			{
				errSR := sendReq(
					client, req, "GET", "/v1/config/stages/"+url.PathEscape(pkg.Name)+"/"+url.PathEscape(pkg.ActiveStage),
					nil, &files,
				)
				if errSR != nil {
					fmt.Fprintln(os.Stderr, errSR.Error())
					os.Exit(1)
				}
			}

			uploadFiles := map[string]string{}

			for _, file := range files.Results {
				if file.Type == "file" && strings.Contains(file.Name, "/") {
					var content []byte

					{
						/*
							steps := strings.Split(file.Name, "/")
							for i, step := range steps {
								steps[i] = url.PathEscape(step)
							}
						*/

						errSR := sendReq(
							client, req,
							"GET", "/v1/config/files/"+url.PathEscape(pkg.Name)+"/"+
								url.PathEscape(pkg.ActiveStage)+"/"+file.Name, //+strings.Join(steps, "/"),
							nil, &content,
						)
						if errSR != nil {
							fmt.Fprintln(os.Stderr, errSR.Error())
							os.Exit(1)
						}
					}

					uploadFiles[file.Name] = string(content)
				}
			}

			if len(uploadFiles) > 0 {
				f, errOp := os.Create(url.PathEscape(pkg.Name) + ".json")
				if errOp != nil {
					fmt.Fprintln(os.Stderr, errOp.Error())
					os.Exit(1)
				}

				buf := bufio.NewWriter(f)

				errEc := json.NewEncoder(buf).Encode(&struct {
					Files map[string]string `json:"files"`
				}{uploadFiles})
				if errEc != nil {
					fmt.Fprintln(os.Stderr, errEc.Error())
					os.Exit(1)
				}

				if errFl := buf.Flush(); errFl != nil {
					fmt.Fprintln(os.Stderr, errFl.Error())
					os.Exit(1)
				}

				if errCl := f.Close(); errCl != nil {
					fmt.Fprintln(os.Stderr, errCl.Error())
					os.Exit(1)
				}
			}
		}
	}
}

func sendReq(client *http.Client, base *http.Request, method, uri string, in, out interface{}) error {
	req := *base
	url := *req.URL

	req.Method = method
	req.URL = &url
	url.Path = uri

	if in != nil {
		buf := &bytes.Buffer{}
		if errEc := json.NewEncoder(buf).Encode(in); errEc != nil {
			return errEc
		}

		req.Body = closableReader{buf}
	}

	resp, errDo := client.Do(&req)
	if errDo != nil {
		return errDo
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		io.Copy(os.Stderr, resp.Body)
		return badHttpStatus{resp.StatusCode}
	}

	if out != nil {
		if bs, ok := out.(*[]byte); ok {
			body, errRA := ioutil.ReadAll(resp.Body)
			if errRA != nil {
				return errRA
			}

			*bs = body
		} else if errDc := json.NewDecoder(bufio.NewReader(resp.Body)).Decode(out); errDc != nil {
			return errDc
		}
	}

	return nil
}
