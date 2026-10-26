//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	apiTimeout   = 60 * time.Second
	readyTimeout = 5 * time.Minute
	goneTimeout  = 3 * time.Minute
)

var _ = Describe("stack", Ordered, func() {
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpClusterState(CurrentSpecReport().FullText())
		}
	})

	// The releases environments/kind.yaml leaves on.
	DescribeTable("has the release deployed",
		func(namespace, release string) {
			out, err := helm("status", release, "-n", namespace, "-o", "json")
			Expect(err).NotTo(HaveOccurred(), out)
			var st struct {
				Info struct {
					Status string `json:"status"`
				} `json:"info"`
			}
			Expect(json.Unmarshal([]byte(out), &st)).To(Succeed())
			Expect(st.Info.Status).To(Equal("deployed"))
		},
		Entry(nil, routeNS, "openresty"),
		Entry(nil, routeNS, "autoconfig"),
		Entry(nil, routeNS, "bodylog"),
		Entry(nil, routeNS, "bodylog-exporter"),
		Entry(nil, routeNS, "bodylog-victorialogs"),
		Entry(nil, operatorNS, "llmscaleoperator"),
		Entry(nil, sloNS, "llm-slo"),
		Entry(nil, consoleNS, "swissd"),
		Entry(nil, consoleNS, "console"),
	)

	It("logs in to console with the default admin", func() {
		form := url.Values{
			"grant_type":    {"password"},
			"username":      {"admin"},
			"password":      {"P@88w0rd"},
			"client_id":     {"kubesphere"},
			"client_secret": {"kubesphere"},
		}
		var tok struct {
			AccessToken string `json:"access_token"`
		}
		code, body := call(http.MethodPost, console.base()+"/oauth/token", "", "application/x-www-form-urlencoded",
			strings.NewReader(form.Encode()), &tok)
		Expect(code).To(Equal(http.StatusOK), body)
		Expect(tok.AccessToken).NotTo(BeEmpty())
	})

	Describe("swissd", Ordered, func() {
		var token string

		DescribeTable("answers its probes",
			func(path string) {
				code, body := call(http.MethodGet, swissd.base()+path, "", "", nil, nil)
				Expect(code).To(Equal(http.StatusOK), body)
			},
			Entry(nil, "/healthz"),
			Entry(nil, "/readyz"),
		)

		It("logs in with the swissd-auth Secret", func() {
			user := secretValue(consoleNS, "swissd-auth", "username")
			pass := secretValue(consoleNS, "swissd-auth", "password")
			var resp struct {
				Token string `json:"token"`
			}
			mustAPI(token, http.MethodPost, "/api/login", map[string]string{"username": user, "password": pass}, &resp)
			Expect(resp.Token).NotTo(BeEmpty())
			token = resp.Token
		})

		It("saves the kind site profile", func() {
			b, err := os.ReadFile(filepath.Join(root, "test/e2e/testdata/profile.yaml"))
			Expect(err).NotTo(HaveOccurred())
			mustAPI(token, http.MethodPut, "/api/profile", map[string]string{"yaml": string(b)}, nil)

			Eventually(func(g Gomega) {
				var info struct {
					Name       string   `json:"name"`
					Warnings   []string `json:"warnings"`
					LLMService *struct {
						Available bool `json:"available"`
					} `json:"llmservice"`
				}
				code, body := call(http.MethodGet, swissd.base()+"/api/cluster", token, "", nil, &info)
				g.Expect(code).To(Equal(http.StatusOK), body)
				g.Expect(info.Name).To(Equal("kind-e2e"), "warnings: %v", info.Warnings)
				g.Expect(info.LLMService).NotTo(BeNil(), "warnings: %v", info.Warnings)
				g.Expect(info.LLMService.Available).To(BeTrue(),
					"llm-operator's LLMService CRD is not available; warnings: %v", info.Warnings)
			}).WithTimeout(apiTimeout).Should(Succeed())
		})

		It("lists the mock model in the catalog", func() {
			Eventually(func(g Gomega) {
				var list struct {
					Models []struct {
						Name string `json:"name"`
					} `json:"models"`
				}
				code, body := call(http.MethodGet, swissd.base()+"/api/catalog", token, "", nil, &list)
				g.Expect(code).To(Equal(http.StatusOK), body)
				var names []string
				for _, m := range list.Models {
					names = append(names, m.Name)
				}
				g.Expect(names).To(ContainElement(cfg.model))
			}).WithTimeout(apiTimeout).Should(Succeed())
		})

		// Each backend: install through swissd, chat with the engine directly
		// and through openresty, find the routed request in victorialogs,
		// then uninstall.
		DescribeTable("deploys, serves and removes the mock model",
			func(backend string) {
				rel := "e2e-mock-" + backend
				DeferCleanup(func() { removeModel(token, rel) })
				deployModel(token, backend, rel)

				By("chatting with the engine Service")
				proxy := fmt.Sprintf("/api/v1/namespaces/%s/services/http:%s:8081/proxy", modelNS, rel)
				var models modelList
				Expect(json.Unmarshal([]byte(mustKubectl("get", "--raw", proxy+"/v1/models")), &models)).To(Succeed())
				Expect(models.ids()).To(ContainElement(cfg.model))

				By("chatting through openresty at /" + rel + "/")
				nonce := fmt.Sprintf("e2e%s%d", strings.ReplaceAll(backend, "-", ""), time.Now().UnixNano())
				Eventually(func(g Gomega) {
					var got modelList
					code, body := call(http.MethodGet, openresty.base()+"/"+rel+"/v1/models", "", "", nil, &got)
					g.Expect(code).To(Equal(http.StatusOK), body)
					g.Expect(got.ids()).To(ContainElement(cfg.model))
				}).WithTimeout(readyTimeout).WithPolling(2 * time.Second).Should(Succeed())
				var chat chatResponse
				code, body := call(http.MethodPost, openresty.base()+"/"+rel+"/v1/chat/completions", "", "application/json",
					jsonBody(chatRequest(cfg.model, nonce)), &chat)
				Expect(code).To(Equal(http.StatusOK), body)
				Expect(chat.Choices).NotTo(BeEmpty())
				Expect(chat.Choices[0].Message.Content).NotTo(BeEmpty())

				// Each poll sends the request again: the bodylog-victorialogs Vector
				// was seen holding the newest record until the next one was appended
				// (minutes, on a quiet cluster), so a single request can sit unshipped.
				By("finding the request in victorialogs")
				Eventually(func(g Gomega) {
					code, body := call(http.MethodPost, openresty.base()+"/"+rel+"/v1/chat/completions", "", "application/json",
						jsonBody(chatRequest(cfg.model, nonce)), nil)
					g.Expect(code).To(Equal(http.StatusOK), body)
					q := url.Values{"query": {"req_body:" + nonce}, "limit": {"5"}}
					code, body = call(http.MethodPost, victorialogs.base()+"/select/logsql/query", "",
						"application/x-www-form-urlencoded", strings.NewReader(q.Encode()), nil)
					g.Expect(code).To(Equal(http.StatusOK), body)
					g.Expect(body).To(ContainSubstring(nonce))
					g.Expect(body).To(ContainSubstring(`"model":"` + cfg.model + `"`))
				}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
			},
			Entry(nil, "helm"),
			Entry(nil, "llmsvc"),
		)
	})
})

// deployModel plans and installs the mock model with swissd, then waits for
// its pod and its ModelRoute.
func deployModel(token, backend, rel string) {
	By(fmt.Sprintf("[%s] planning %s as %s/%s", backend, cfg.model, modelNS, rel))
	var planned struct {
		Hash string `json:"hash"`
	}
	// Nothing creates the model namespace beforehand: swissd, installed at
	// cluster scope, creates it on install.
	mustAPI(token, http.MethodPost, "/api/plans", map[string]any{
		"model": cfg.model, "release": rel, "namespace": modelNS, "serviceId": rel,
		"createNamespace": true,
	}, &planned)
	Expect(planned.Hash).NotTo(BeEmpty())

	By("[" + backend + "] installing")
	var installed struct {
		Status      string `json:"status"`
		StatusError string `json:"statusError"`
	}
	mustAPI(token, http.MethodPost, "/api/install", map[string]string{
		"planHash": planned.Hash, "applyWith": backend,
	}, &installed)
	Expect(installed.Status).To(Equal("applied"), installed.StatusError)

	By("[" + backend + "] waiting for the release to be ready")
	Eventually(func(g Gomega) {
		var st struct {
			Ready int    `json:"ready"`
			Total int    `json:"total"`
			Route string `json:"route"`
		}
		code, body := call(http.MethodGet, fmt.Sprintf("%s/api/releases/%s/%s/status", swissd.base(), modelNS, rel), token, "", nil, &st)
		g.Expect(code).To(Equal(http.StatusOK), body)
		g.Expect(st.Total).To(BeNumerically(">", 0))
		g.Expect(st.Ready).To(Equal(st.Total))
		g.Expect(st.Route).To(Equal(rel))
	}).WithTimeout(readyTimeout).WithPolling(5 * time.Second).Should(Succeed())

	Eventually(func(g Gomega) {
		out, err := kubectl("-n", modelNS, "get", "modelroute", rel,
			"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
		g.Expect(err).NotTo(HaveOccurred(), out)
		g.Expect(out).To(Equal("True"))
	}).WithTimeout(readyTimeout).WithPolling(2 * time.Second).Should(Succeed())
}

// removeModel uninstalls through swissd and waits for its objects to go. A
// 404 is fine: a deploy that failed early may have left nothing to remove.
func removeModel(token, rel string) {
	By("uninstalling " + rel)
	code, body := call(http.MethodDelete, fmt.Sprintf("%s/api/releases/%s/%s", swissd.base(), modelNS, rel), token, "", nil, nil)
	Expect(code).To(BeElementOf(http.StatusOK, http.StatusNotFound), body)
	Eventually(func(g Gomega) {
		out, err := kubectl("-n", modelNS, "get", "deploy,svc,pods,llmservices,modelroutes,llmscalers", "-o", "name")
		g.Expect(err).NotTo(HaveOccurred(), out)
		for _, obj := range nonEmptyLines(out) {
			name := obj[strings.LastIndex(obj, "/")+1:]
			g.Expect(name == rel || strings.HasPrefix(name, rel+"-")).To(BeFalse(), "%s is still there", obj)
		}
	}).WithTimeout(goneTimeout).WithPolling(3 * time.Second).Should(Succeed())
}

func secretValue(ns, name, key string) string {
	out := mustKubectl("-n", ns, "get", "secret", name, "-o", "jsonpath={.data."+key+"}")
	b, err := base64.StdEncoding.DecodeString(out)
	Expect(err).NotTo(HaveOccurred())
	Expect(b).NotTo(BeEmpty(), "secret %s/%s has no %s", ns, name, key)
	return string(b)
}

// mustAPI calls swissd and expects 200.
func mustAPI(token, method, path string, in, out any) {
	var body io.Reader
	ctype := ""
	if in != nil {
		body, ctype = jsonBody(in), "application/json"
	}
	code, text := call(method, swissd.base()+path, token, ctype, body, out)
	ExpectWithOffset(1, code).To(Equal(http.StatusOK), "%s %s: %s", method, path, text)
}

// httpClient opens a connection per request. kubectl port-forward drops idle
// ones, and Go does not retry a DELETE that hits the closed socket (seen: EOF
// on the uninstall after a minute without a swissd call).
var httpClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// call does one HTTP request and decodes a 2xx JSON body into out when given.
func call(method, target, token, ctype string, body io.Reader, out any) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode/100 == 2 {
		ExpectWithOffset(1, json.Unmarshal(b, out)).To(Succeed(), string(b))
	}
	return resp.StatusCode, string(b)
}

func jsonBody(v any) io.Reader {
	b, err := json.Marshal(v)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return bytes.NewReader(b)
}

type modelList struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (m modelList) ids() []string {
	var ids []string
	for _, d := range m.Data {
		ids = append(ids, d.ID)
	}
	return ids
}

func chatRequest(model, content string) map[string]any {
	return map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": content}},
	}
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}
