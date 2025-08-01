/*
Copyright 2025 The llm-d-inference-sim Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package llmdinferencesim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/valyala/fasthttp/fasthttputil"
	"k8s.io/klog/v2"
)

const model = "my_model"
const baseURL = "http://localhost/v1"
const userMessage = "This is a test."
const invalidMaxTokensErrMsg = "Max completion tokens and max tokens should be positive"

var userMsgTokens int64

func startServer(ctx context.Context, mode string) (*http.Client, error) {
	return startServerWithArgs(ctx, mode, nil)
}

func startServerWithArgs(ctx context.Context, mode string, args []string) (*http.Client, error) {
	oldArgs := os.Args
	defer func() {
		os.Args = oldArgs
	}()

	if args != nil {
		os.Args = args
	} else {
		os.Args = []string{"cmd", "--model", model, "--mode", mode}
	}
	logger := klog.Background()

	s, err := New(logger)
	if err != nil {
		return nil, err
	}
	// parse command line parameters
	if err := s.parseCommandParamsAndLoadConfig(); err != nil {
		return nil, err
	}

	// calculate number of tokens for user message,
	// must be activated after parseCommandParamsAndLoadConfig since it initializes the random engine
	userMsgTokens = int64(len(tokenize(userMessage)))

	// run request processing workers
	for i := 1; i <= s.config.MaxNumSeqs; i++ {
		go s.reqProcessingWorker(ctx, i)
	}

	listener := fasthttputil.NewInmemoryListener()

	// start the http server
	go func() {
		if err := s.startServer(listener); err != nil {
			logger.Error(err, "error starting server")
		}
	}()

	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return listener.Dial()
			},
		},
	}, nil
}

var _ = Describe("Simulator", func() {

	DescribeTable("chat completions streaming",
		func(mode string) {
			ctx := context.TODO()
			client, err := startServer(ctx, mode)
			Expect(err).NotTo(HaveOccurred())

			openaiclient := openai.NewClient(
				option.WithBaseURL(baseURL),
				option.WithHTTPClient(client))

			params := openai.ChatCompletionNewParams{
				Messages: []openai.ChatCompletionMessageParamUnion{
					openai.UserMessage(userMessage),
				},
				Model:         model,
				StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: param.NewOpt(true)},
			}
			stream := openaiclient.Chat.Completions.NewStreaming(ctx, params)
			defer func() {
				err := stream.Close()
				Expect(err).NotTo(HaveOccurred())
			}()
			tokens := []string{}
			role := ""
			var chunk openai.ChatCompletionChunk
			numberOfChunksWithUsage := 0
			for stream.Next() {
				chunk = stream.Current()
				for _, choice := range chunk.Choices {
					if choice.Delta.Role != "" {
						role = choice.Delta.Role
					} else if choice.FinishReason == "" {
						tokens = append(tokens, choice.Delta.Content)
					}
				}
				if chunk.Usage.CompletionTokens != 0 || chunk.Usage.PromptTokens != 0 || chunk.Usage.TotalTokens != 0 {
					numberOfChunksWithUsage++
				}
				Expect(string(chunk.Object)).To(Equal(chatCompletionChunkObject))
			}

			Expect(numberOfChunksWithUsage).To(Equal(1))
			Expect(chunk.Usage.PromptTokens).To(Equal(userMsgTokens))
			Expect(chunk.Usage.CompletionTokens).To(BeNumerically(">", 0))
			Expect(chunk.Usage.TotalTokens).To(Equal(chunk.Usage.PromptTokens + chunk.Usage.CompletionTokens))

			msg := strings.Join(tokens, "")
			if mode == modeRandom {
				// in case of random mode ensure that the returned message could be output of the random text generator
				Expect(isValidText(msg)).To(BeTrue())
			} else {
				// in case of echo mode check that the text is returned as-is
				Expect(msg).Should(Equal(userMessage))
			}
			Expect(role).Should(Equal("assistant"))
		},
		func(mode string) string {
			return "mode: " + mode
		},
		Entry(nil, modeRandom),
		Entry(nil, modeEcho),
	)

	DescribeTable("text completions streaming",
		func(mode string) {
			ctx := context.TODO()
			client, err := startServer(ctx, mode)
			Expect(err).NotTo(HaveOccurred())

			openaiclient := openai.NewClient(
				option.WithBaseURL(baseURL),
				option.WithHTTPClient(client))

			params := openai.CompletionNewParams{
				Prompt: openai.CompletionNewParamsPromptUnion{
					OfString: openai.String(userMessage),
				},
				Model:         openai.CompletionNewParamsModel(model),
				StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: param.NewOpt(true)},
			}
			stream := openaiclient.Completions.NewStreaming(ctx, params)
			defer func() {
				err := stream.Close()
				Expect(err).NotTo(HaveOccurred())
			}()
			tokens := []string{}
			var chunk openai.Completion
			numberOfChunksWithUsage := 0
			for stream.Next() {
				chunk = stream.Current()
				for _, choice := range chunk.Choices {
					if choice.FinishReason == "" {
						tokens = append(tokens, choice.Text)
					}
				}
				if chunk.Usage.CompletionTokens != 0 || chunk.Usage.PromptTokens != 0 || chunk.Usage.TotalTokens != 0 {
					numberOfChunksWithUsage++
				}
				Expect(string(chunk.Object)).To(Equal(textCompletionObject))
			}
			Expect(numberOfChunksWithUsage).To(Equal(1))
			Expect(chunk.Usage.PromptTokens).To(Equal(userMsgTokens))
			Expect(chunk.Usage.CompletionTokens).To(BeNumerically(">", 0))
			Expect(chunk.Usage.TotalTokens).To(Equal(chunk.Usage.PromptTokens + chunk.Usage.CompletionTokens))

			text := strings.Join(tokens, "")
			if mode == modeRandom {
				// in case of random mode ensure that the returned message could be output of the random text generator
				Expect(isValidText(text)).To(BeTrue())
			} else {
				// in case of echo mode check that the text is returned as-is
				Expect(text).Should(Equal(userMessage))
			}
		},
		func(mode string) string {
			return "mode: " + mode
		},
		Entry(nil, modeRandom),
		Entry(nil, modeEcho),
	)

	DescribeTable("chat completions",
		func(mode string, maxTokens int, maxCompletionTokens int) {
			ctx := context.TODO()
			client, err := startServer(ctx, mode)
			Expect(err).NotTo(HaveOccurred())

			openaiclient := openai.NewClient(
				option.WithBaseURL(baseURL),
				option.WithHTTPClient(client))

			params := openai.ChatCompletionNewParams{
				Messages: []openai.ChatCompletionMessageParamUnion{
					openai.UserMessage(userMessage),
				},
				Model: model,
			}
			numTokens := 0
			// if maxTokens and maxCompletionTokens are passsed
			// maxCompletionTokens is used
			if maxTokens != 0 {
				params.MaxTokens = param.NewOpt(int64(maxTokens))
				numTokens = maxTokens
			}
			if maxCompletionTokens != 0 {
				params.MaxCompletionTokens = param.NewOpt(int64(maxCompletionTokens))
				numTokens = maxCompletionTokens
			}
			resp, err := openaiclient.Chat.Completions.New(ctx, params)
			if err != nil {
				var openaiError *openai.Error
				if errors.As(err, &openaiError) {
					if openaiError.StatusCode == 400 {
						errMsg, err := io.ReadAll(openaiError.Response.Body)
						Expect(err).NotTo(HaveOccurred())
						if strings.Contains(string(errMsg), invalidMaxTokensErrMsg) {
							return
						}
					}
				}
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Choices).ShouldNot(BeEmpty())
			Expect(string(resp.Object)).To(Equal(chatCompletionObject))

			Expect(resp.Usage.PromptTokens).To(Equal(userMsgTokens))
			Expect(resp.Usage.CompletionTokens).To(BeNumerically(">", 0))
			Expect(resp.Usage.TotalTokens).To(Equal(resp.Usage.PromptTokens + resp.Usage.CompletionTokens))

			msg := resp.Choices[0].Message.Content
			Expect(msg).ShouldNot(BeEmpty())

			if numTokens > 0 {
				tokens := tokenize(msg)
				Expect(int64(len(tokens))).Should(BeNumerically("<=", numTokens))
			} else {
				if mode == modeRandom {
					// in case of random mode ensure that the returned message could be output of the random text generator
					Expect(isValidText(msg)).To(BeTrue())
				} else {
					// in case of echo mode check that the text is returned as-is
					Expect(msg).Should(Equal(userMessage))
				}
			}
		},
		func(mode string, maxTokens int, maxCompletionTokens int) string {
			return fmt.Sprintf("mode: %s max_tokens: %d max_completion_tokens: %d", mode, maxTokens, maxCompletionTokens)
		},
		Entry(nil, modeRandom, 2, 0),
		Entry(nil, modeEcho, 2, 0),
		Entry(nil, modeRandom, 1000, 0),
		Entry(nil, modeEcho, 1000, 0),
		Entry(nil, modeRandom, 1000, 2),
		Entry(nil, modeEcho, 1000, 2),
		Entry(nil, modeRandom, 0, 2),
		Entry(nil, modeEcho, 0, 2),
		Entry(nil, modeRandom, 0, 1000),
		Entry(nil, modeEcho, 0, 1000),
		Entry(nil, modeRandom, 0, 0),
		Entry(nil, modeEcho, 0, 0),
		Entry(nil, modeRandom, -1, 0),
		Entry(nil, modeEcho, -1, 0),
		Entry(nil, modeRandom, 0, -1),
		Entry(nil, modeEcho, 0, -1),
	)

	DescribeTable("text completions",
		// use a function so that httpClient is captured when running
		func(mode string, maxTokens int) {
			ctx := context.TODO()
			client, err := startServer(ctx, mode)
			Expect(err).NotTo(HaveOccurred())

			openaiclient := openai.NewClient(
				option.WithBaseURL(baseURL),
				option.WithHTTPClient(client))
			params := openai.CompletionNewParams{
				Prompt: openai.CompletionNewParamsPromptUnion{
					OfString: openai.String(userMessage),
				},
				Model: openai.CompletionNewParamsModel(model),
			}
			numTokens := 0
			if maxTokens != 0 {
				params.MaxTokens = param.NewOpt(int64(maxTokens))
				numTokens = maxTokens
			}
			resp, err := openaiclient.Completions.New(ctx, params)
			if err != nil {
				var openaiError *openai.Error
				if errors.As(err, &openaiError) {
					if openaiError.StatusCode == 400 {
						errMsg, err := io.ReadAll(openaiError.Response.Body)
						Expect(err).NotTo(HaveOccurred())
						if strings.Contains(string(errMsg), invalidMaxTokensErrMsg) {
							return
						}
					}
				}
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Choices).ShouldNot(BeEmpty())
			Expect(string(resp.Object)).To(Equal(textCompletionObject))

			Expect(resp.Usage.PromptTokens).To(Equal(userMsgTokens))
			Expect(resp.Usage.CompletionTokens).To(BeNumerically(">", 0))
			Expect(resp.Usage.TotalTokens).To(Equal(resp.Usage.PromptTokens + resp.Usage.CompletionTokens))

			text := resp.Choices[0].Text
			Expect(text).ShouldNot(BeEmpty())

			if numTokens != 0 {
				tokens := tokenize(text)
				Expect(int64(len(tokens))).Should(BeNumerically("<=", numTokens))
			} else {
				if mode == modeRandom {
					// in case of random mode ensure that the returned message could be output of the random text generator
					Expect(isValidText(text)).To(BeTrue())
				} else {
					// in case of echo mode check that the text is returned as-is
					Expect(text).Should(Equal(userMessage))
				}
			}
		},
		func(mode string, maxTokens int) string {
			return fmt.Sprintf("mode: %s max_tokens: %d", mode, maxTokens)
		},
		Entry(nil, modeRandom, 2),
		Entry(nil, modeEcho, 2),
		Entry(nil, modeRandom, 1000),
		Entry(nil, modeEcho, 1000),
		Entry(nil, modeRandom, 0),
		Entry(nil, modeEcho, 0),
		Entry(nil, modeRandom, -1),
		Entry(nil, modeEcho, -1),
	)

	It("Should respond to /health", func() {
		ctx := context.TODO()
		client, err := startServer(ctx, modeRandom)
		Expect(err).NotTo(HaveOccurred())

		resp, err := client.Get("http://localhost/health")
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})

	It("Should respond to /ready", func() {
		ctx := context.TODO()
		client, err := startServer(ctx, modeRandom)
		Expect(err).NotTo(HaveOccurred())

		resp, err := client.Get("http://localhost/ready")
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})

	Context("max-model-len context window validation", func() {
		It("Should reject requests exceeding context window", func() {
			ctx := context.TODO()
			// Start server with max-model-len=10
			args := []string{"cmd", "--model", model, "--mode", modeRandom, "--max-model-len", "10"}
			client, err := startServerWithArgs(ctx, modeRandom, args)
			Expect(err).NotTo(HaveOccurred())

			// Test with raw HTTP to verify the error response format
			reqBody := `{
				"messages": [{"role": "user", "content": "This is a test message"}],
				"model": "my_model",
				"max_tokens": 8
			}`

			resp, err := client.Post("http://localhost/v1/chat/completions", "application/json", strings.NewReader(reqBody))
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := resp.Body.Close()
				Expect(err).NotTo(HaveOccurred())
			}()

			body, err := io.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())

			Expect(resp.StatusCode).To(Equal(400))
			Expect(string(body)).To(ContainSubstring("This model's maximum context length is 10 tokens"))
			Expect(string(body)).To(ContainSubstring("However, you requested 13 tokens"))
			Expect(string(body)).To(ContainSubstring("5 in the messages, 8 in the completion"))
			Expect(string(body)).To(ContainSubstring("BadRequestError"))

			// Also test with OpenAI client to ensure it gets an error
			openaiclient := openai.NewClient(
				option.WithBaseURL(baseURL),
				option.WithHTTPClient(client),
			)

			_, err = openaiclient.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
				Messages: []openai.ChatCompletionMessageParamUnion{
					openai.UserMessage("This is a test message"),
				},
				Model:     model,
				MaxTokens: openai.Int(8),
			})

			Expect(err).To(HaveOccurred())
			var apiErr *openai.Error
			Expect(errors.As(err, &apiErr)).To(BeTrue())
			Expect(apiErr.StatusCode).To(Equal(400))
		})

		It("Should accept requests within context window", func() {
			ctx := context.TODO()
			// Start server with max-model-len=50
			args := []string{"cmd", "--model", model, "--mode", modeEcho, "--max-model-len", "50"}
			client, err := startServerWithArgs(ctx, modeEcho, args)
			Expect(err).NotTo(HaveOccurred())

			openaiclient := openai.NewClient(
				option.WithBaseURL(baseURL),
				option.WithHTTPClient(client),
			)

			// Send a request within the context window
			resp, err := openaiclient.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
				Messages: []openai.ChatCompletionMessageParamUnion{
					openai.UserMessage("Hello"),
				},
				Model:     model,
				MaxTokens: openai.Int(5),
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Choices).To(HaveLen(1))
			Expect(resp.Model).To(Equal(model))
		})

		It("Should handle text completion requests exceeding context window", func() {
			ctx := context.TODO()
			// Start server with max-model-len=10
			args := []string{"cmd", "--model", model, "--mode", modeRandom, "--max-model-len", "10"}
			client, err := startServerWithArgs(ctx, modeRandom, args)
			Expect(err).NotTo(HaveOccurred())

			// Test with raw HTTP for text completion
			reqBody := `{
				"prompt": "This is a long test prompt with many words",
				"model": "my_model",
				"max_tokens": 5
			}`

			resp, err := client.Post("http://localhost/v1/completions", "application/json", strings.NewReader(reqBody))
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := resp.Body.Close()
				Expect(err).NotTo(HaveOccurred())
			}()

			body, err := io.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())

			Expect(resp.StatusCode).To(Equal(400))
			Expect(string(body)).To(ContainSubstring("This model's maximum context length is 10 tokens"))
			Expect(string(body)).To(ContainSubstring("BadRequestError"))
		})
	})

	Describe("Check random latencies", Ordered, func() {
		var simulator *VllmSimulator

		BeforeAll(func() {
			var err error
			simulator, err = New(klog.Background())
			Expect(err).NotTo(HaveOccurred())

			simulator.config = newConfig()
			simulator.config.TimeToFirstToken = 2048
			simulator.config.TimeToFirstTokenStdDev = 2048
			simulator.config.KVCacheTransferLatency = 2048
			simulator.config.KVCacheTransferLatencyStdDev = 2048
		})

		DescribeTable("should calculate inter token latency correctly",
			func(interTokenLatency int, stddev int) {
				simulator.config.InterTokenLatency = interTokenLatency
				simulator.config.InterTokenLatencyStdDev = stddev
				interToken := simulator.getInterTokenLatency()
				Expect(interToken).To(BeNumerically(">=", float32(interTokenLatency)*0.3))
				Expect(interToken).To(BeNumerically("<=", float32(interTokenLatency)*1.7))
			},
			func(interTokenLatency int, stddev int) string {
				return fmt.Sprintf("interTokenLatency: %d stddev: %d", interTokenLatency, stddev)
			},
			Entry(nil, 1000, 300),
			Entry(nil, 1000, 800), // invalid std dev, used for testing purposes
			Entry(nil, 1000, 900), // invalid std dev, used for testing purposes
			Entry(nil, 1000, 0),
		)

		DescribeTable("should calculate total inter token latency correctly",
			func(interTokenLatency int, stddev int, numberOfTokens int) {
				simulator.config.InterTokenLatency = interTokenLatency
				simulator.config.InterTokenLatencyStdDev = stddev
				latency := simulator.getTotalInterTokenLatency(numberOfTokens)
				Expect(latency).To(BeNumerically(">=", float32(interTokenLatency)*0.3*float32(numberOfTokens)))
				Expect(latency).To(BeNumerically("<=", float32(interTokenLatency)*1.7*float32(numberOfTokens)))
			},
			func(interTokenLatency int, stddev int, numberOfTokens int) string {
				return fmt.Sprintf("interTokenLatency: %d stddev: %d, numberOfTokens: %d", interTokenLatency,
					stddev, numberOfTokens)
			},
			Entry(nil, 1000, 30, 100),
			Entry(nil, 1000, 800, 20), // invalid std dev, used for testing purposes
			Entry(nil, 1000, 900, 5),  // invalid std dev, used for testing purposes
			Entry(nil, 1000, 0, 50),
		)

		DescribeTable("should calculate time to first token correctly",
			func(timeToFirstToken int, timeToFirstTokenStdDev int,
				kvCacheLatency int, kvCacheLatencyStdDev int, doREmotePrefill bool) {
				simulator.config.TimeToFirstToken = timeToFirstToken
				simulator.config.TimeToFirstTokenStdDev = timeToFirstTokenStdDev
				simulator.config.KVCacheTransferLatency = kvCacheLatency
				simulator.config.KVCacheTransferLatencyStdDev = kvCacheLatencyStdDev
				timeToFirst := simulator.getTimeToFirstToken(doREmotePrefill)
				if doREmotePrefill {
					Expect(timeToFirst).To(BeNumerically(">=", float32(kvCacheLatency)*0.3))
					Expect(timeToFirst).To(BeNumerically("<=", float32(kvCacheLatency)*1.7))
				} else {
					Expect(timeToFirst).To(BeNumerically(">=", float32(timeToFirstToken)*0.3))
					Expect(timeToFirst).To(BeNumerically("<=", float32(timeToFirstToken)*1.7))
				}
			},
			func(timeToFirstToken int, timeToFirstTokenStdDev int,
				kvCacheLatency int, kvCacheLatencyStdDev int, doREmotePrefill bool) string {
				return fmt.Sprintf("timeToFirstToken: %d stddev: %d kvCacheLatency: %d stddev: %d doREmotePrefill: %t",
					timeToFirstToken, timeToFirstTokenStdDev, kvCacheLatency, kvCacheLatencyStdDev, doREmotePrefill)
			},
			Entry(nil, 10000, 300, 1000, 200, true),
			Entry(nil, 10000, 300, 1000, 200, false),
			Entry(nil, 10000, 9000, 1000, 800, true),  // invalid std dev, used for testing purposes
			Entry(nil, 10000, 8000, 1000, 900, false), // invalid std dev, used for testing purposes
			Entry(nil, 10000, 0, 1000, 0, true),
			Entry(nil, 10000, 0, 1000, 0, false),
		)
	})
})
