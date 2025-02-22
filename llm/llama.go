package llm

/*
#cgo CFLAGS: -Ofast -std=c11 -fPIC
#cgo CPPFLAGS: -Ofast -Wall -Wextra -Wno-unused-function -Wno-unused-variable -DNDEBUG -DGGML_USE_K_QUANTS
#cgo CXXFLAGS: -std=c++11 -fPIC
#cgo darwin CPPFLAGS:  -DGGML_USE_ACCELERATE
#cgo darwin,arm64 CPPFLAGS: -DGGML_USE_METAL -DGGML_METAL_NDEBUG
#cgo darwin LDFLAGS: -framework Accelerate -framework Foundation -framework Metal -framework MetalKit -framework MetalPerformanceShaders
#include <stdlib.h>
#include "llama.h"

struct llama_sample_options
{
	float repeat_penalty;
	float frequency_penalty;
	float presence_penalty;
	float temperature;
	int32_t top_k;
	float top_p;
	float tfs_z;
	float typical_p;
	int mirostat;
	float mirostat_tau;
	float mirostat_eta;
	bool penalize_newline;
};

llama_token llama_sample(
		struct llama_context *ctx,
		struct llama_token_data *candidates,
		size_t n_candidates,
		const llama_token *last_tokens,
		size_t n_last_tokens,
		struct llama_sample_options *opts)
{
	llama_token_data_array candidates_p = {
		candidates,
		n_candidates,
		false,
	};

	struct llama_token_data newline = candidates_p.data[llama_token_nl()];

	llama_sample_repetition_penalty(
		ctx, &candidates_p,
		last_tokens, n_last_tokens,
		opts->repeat_penalty);

	llama_sample_frequency_and_presence_penalties(
		ctx, &candidates_p,
		last_tokens, n_last_tokens,
		opts->frequency_penalty, opts->presence_penalty);

	if (!opts->penalize_newline) {
		candidates_p.data[llama_token_nl()] = newline;
	}

	if (opts->temperature <= 0) {
		return llama_sample_token_greedy(ctx, &candidates_p);
	}

	if (opts->mirostat == 1) {
		int mirostat_m = 100;
		float mirostat_mu = 2.0f * opts->mirostat_tau;
		llama_sample_temperature(ctx, &candidates_p, opts->temperature);
		return llama_sample_token_mirostat(
			ctx, &candidates_p,
			opts->mirostat_tau, opts->mirostat_eta,
			mirostat_m, &mirostat_mu);
	} else if (opts->mirostat == 2) {
		float mirostat_mu = 2.0f * opts->mirostat_tau;
		llama_sample_temperature(ctx, &candidates_p, opts->temperature);
		return llama_sample_token_mirostat_v2(
			ctx, &candidates_p,
			opts->mirostat_tau, opts->mirostat_eta,
			&mirostat_mu);
	} else {
		llama_sample_top_k(ctx, &candidates_p, opts->top_k, 1);
		llama_sample_tail_free(ctx, &candidates_p, opts->tfs_z, 1);
		llama_sample_typical(ctx, &candidates_p, opts->typical_p, 1);
		llama_sample_top_p(ctx, &candidates_p, opts->top_p, 1);
		llama_sample_temperature(ctx, &candidates_p, opts->temperature);
		return llama_sample_token(ctx, &candidates_p);
	}
}
*/
import "C"

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
	"unsafe"

	"github.com/jmorganca/ollama/api"
)

//go:embed ggml-metal.metal
var fs embed.FS

const ModelFamilyLlama ModelFamily = "llama"

type llamaModel struct {
	hyperparameters llamaHyperparameters
}

func (llm *llamaModel) ModelFamily() ModelFamily {
	return ModelFamilyLlama
}

func (llm *llamaModel) ModelType() ModelType {
	return ModelType30B
}

func (llm *llamaModel) FileType() FileType {
	return llm.hyperparameters.FileType
}

type llamaHyperparameters struct {
	// NumVocab is the size of the model's vocabulary.
	NumVocab uint32

	// NumEmbd is the size of the model's embedding layer.
	NumEmbd uint32
	NumMult uint32
	NumHead uint32

	// NumLayer is the number of layers in the model.
	NumLayer uint32
	NumRot   uint32

	// FileType describes the quantization level of the model, e.g. Q4_0, Q5_K, etc.
	FileType llamaFileType
}

type llamaFileType uint32

const (
	llamaFileTypeF32 llamaFileType = iota
	llamaFileTypeF16
	llamaFileTypeQ4_0
	llamaFileTypeQ4_1
	llamaFileTypeQ4_1_F16
	llamaFileTypeQ8_0 llamaFileType = iota + 2
	llamaFileTypeQ5_0
	llamaFileTypeQ5_1
	llamaFileTypeQ2_K
	llamaFileTypeQ3_K_S
	llamaFileTypeQ3_K_M
	llamaFileTypeQ3_K_L
	llamaFileTypeQ4_K_S
	llamaFileTypeQ4_K_M
	llamaFileTypeQ5_K_S
	llamaFileTypeQ5_K_M
	llamaFileTypeQ6_K
)

func (ft llamaFileType) String() string {
	switch ft {
	case llamaFileTypeF32:
		return "F32"
	case llamaFileTypeF16:
		return "F16"
	case llamaFileTypeQ4_0:
		return "Q4_0"
	case llamaFileTypeQ4_1:
		return "Q4_1"
	case llamaFileTypeQ4_1_F16:
		return "Q4_1_F16"
	case llamaFileTypeQ8_0:
		return "Q8_0"
	case llamaFileTypeQ5_0:
		return "Q5_0"
	case llamaFileTypeQ5_1:
		return "Q5_1"
	case llamaFileTypeQ2_K:
		return "Q2_K"
	case llamaFileTypeQ3_K_S:
		return "Q3_K_S"
	case llamaFileTypeQ3_K_M:
		return "Q3_K_M"
	case llamaFileTypeQ3_K_L:
		return "Q3_K_L"
	case llamaFileTypeQ4_K_S:
		return "Q4_K_S"
	case llamaFileTypeQ4_K_M:
		return "Q4_K_M"
	case llamaFileTypeQ5_K_S:
		return "Q5_K_S"
	case llamaFileTypeQ5_K_M:
		return "Q5_K_M"
	case llamaFileTypeQ6_K:
		return "Q6_K"
	default:
		return "Unknown"
	}
}

type llama struct {
	params *C.struct_llama_context_params
	model  *C.struct_llama_model
	ctx    *C.struct_llama_context

	last   []C.llama_token
	embd   []C.llama_token
	cursor int

	mu sync.Mutex
	gc bool

	api.Options
}

func newLlama(model string, adapters []string, opts api.Options) (*llama, error) {
	if _, err := os.Stat(model); err != nil {
		return nil, err
	}

	llm := llama{Options: opts}

	C.llama_backend_init(C.bool(llm.UseNUMA))

	params := C.llama_context_default_params()
	params.seed = C.uint(llm.Seed)
	params.n_ctx = C.int(llm.NumCtx)
	params.n_batch = C.int(llm.NumBatch)
	params.n_gqa = C.int(llm.NumGQA)
	params.n_gpu_layers = C.int(llm.NumGPU)
	params.main_gpu = C.int(llm.MainGPU)
	params.low_vram = C.bool(llm.LowVRAM)
	params.f16_kv = C.bool(llm.F16KV)
	params.logits_all = C.bool(llm.LogitsAll)
	params.vocab_only = C.bool(llm.VocabOnly)
	params.use_mmap = C.bool(llm.UseMMap)
	params.use_mlock = C.bool(llm.UseMLock)
	params.embedding = C.bool(llm.EmbeddingOnly)
	params.rope_freq_base = C.float(llm.RopeFrequencyBase)
	params.rope_freq_scale = C.float(llm.RopeFrequencyScale)

	if len(adapters) > 0 && llm.UseMMap {
		log.Printf("must disable mmap to use lora adapters")
		params.use_mmap = C.bool(false)
	}

	llm.params = &params

	cModel := C.CString(model)
	defer C.free(unsafe.Pointer(cModel))

	llm.model = C.llama_load_model_from_file(cModel, params)
	if llm.model == nil {
		return nil, errors.New("failed to load model")
	}

	llm.ctx = C.llama_new_context_with_model(llm.model, params)
	if llm.ctx == nil {
		return nil, errors.New("failed to create context")
	}

	for _, adapter := range adapters {
		cAdapter := C.CString(adapter)
		defer C.free(unsafe.Pointer(cAdapter))

		if retval := C.llama_model_apply_lora_from_file(llm.model, cAdapter, nil, C.int(llm.NumThread)); retval != 0 {
			return nil, fmt.Errorf("failed to load adapter %s", adapter)
		}
	}

	// warm up the model
	bos := []C.llama_token{C.llama_token_bos()}
	C.llama_eval(llm.ctx, unsafe.SliceData(bos), C.int(len(bos)), 0, C.int(opts.NumThread))
	C.llama_reset_timings(llm.ctx)

	return &llm, nil
}

func (llm *llama) Close() {
	llm.gc = true

	llm.mu.Lock()
	defer llm.mu.Unlock()

	defer C.llama_free_model(llm.model)
	defer C.llama_free(llm.ctx)

	C.llama_print_timings(llm.ctx)
}

func (llm *llama) SetOptions(opts api.Options) {
	llm.Options = opts
}

var errNeedMoreData = errors.New("need more data")

func (llm *llama) Predict(ctx []int, prompt string, fn func(api.GenerateResponse)) error {
	C.llama_reset_timings(llm.ctx)

	llm.marshalPrompt(ctx, prompt)

	C.llama_set_rng_seed(llm.ctx, C.uint(llm.Seed))

	var b bytes.Buffer
	for {
		token, err := llm.next()
		if llm.gc {
			return nil
		} else if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}

		b.WriteString(llm.Decode(int(token)))

		if err := llm.checkStopConditions(b); err != nil {
			if errors.Is(err, io.EOF) {
				break
			} else if errors.Is(err, errNeedMoreData) {
				continue
			}

			return err
		}

		if utf8.Valid(b.Bytes()) || b.Len() >= utf8.UTFMax {
			fn(api.GenerateResponse{Response: b.String()})
			b.Reset()
		}
	}

	embd := make([]int, len(llm.embd))
	for i := range llm.embd {
		embd[i] = int(llm.embd[i])
	}

	timings := C.llama_get_timings(llm.ctx)
	fn(api.GenerateResponse{
		Done:               true,
		Context:            embd,
		SampleCount:        int(timings.n_sample),
		SampleDuration:     parseDurationMs(float64(timings.t_sample_ms)),
		PromptEvalCount:    int(timings.n_p_eval),
		PromptEvalDuration: parseDurationMs(float64(timings.t_p_eval_ms)),
		EvalCount:          int(timings.n_eval),
		EvalDuration:       parseDurationMs(float64(timings.t_eval_ms)),
	})

	return nil
}

func (llm *llama) checkStopConditions(b bytes.Buffer) error {
	for _, stopCondition := range llm.Stop {
		if stopCondition == strings.TrimSpace(b.String()) {
			return io.EOF
		} else if strings.HasPrefix(stopCondition, strings.TrimSpace(b.String())) {
			return errNeedMoreData
		}
	}

	return nil
}

func (llm *llama) marshalPrompt(ctx []int, prompt string) []C.llama_token {
	tokens := append(ctx, llm.Encode(prompt)...)
	if llm.NumKeep < 0 {
		llm.NumKeep = len(tokens)
	}

	cTokens := make([]C.llama_token, len(tokens))
	for i := range tokens {
		cTokens[i] = C.llama_token(tokens[i])
	}

	// min(llm.NumCtx - 4, llm.NumKeep)
	if llm.NumCtx-4 < llm.NumKeep {
		llm.NumKeep = llm.NumCtx - 4
	}

	if len(tokens) >= llm.NumCtx {
		// truncate input
		numLeft := (llm.NumCtx - llm.NumKeep) / 2
		truncated := cTokens[:llm.NumKeep]
		erasedBlocks := (len(cTokens) - llm.NumKeep - numLeft - 1) / numLeft
		truncated = append(truncated, cTokens[llm.NumKeep+erasedBlocks*numLeft:]...)
		copy(llm.last, cTokens[len(cTokens)-llm.NumCtx:])

		cTokens = truncated
		log.Printf("input truncated: num_ctx=%d num_keep=%d num_left=%d num_tokens=%d", llm.NumCtx, llm.NumKeep, numLeft, len(truncated))
	} else {
		llm.last = make([]C.llama_token, llm.NumCtx-len(cTokens))
		llm.last = append(llm.last, cTokens...)
	}

	var i int
	for i = 0; i < len(llm.embd) && i < len(cTokens) && llm.embd[i] == cTokens[i]; i++ {
		// noop
	}

	llm.embd = cTokens
	if i == len(cTokens) {
		// evaluate at least one token to generate logits
		i--
	}

	llm.cursor = i

	log.Printf("prompt: num_past=%d cached=%v eval=%v", i, len(llm.embd[:i]), len(llm.embd[i:]))
	return cTokens
}

func (llm *llama) Encode(prompt string) []int {
	cPrompt := C.CString(prompt)
	defer C.free(unsafe.Pointer(cPrompt))

	cTokens := make([]C.llama_token, len(prompt)+1)
	if n := C.llama_tokenize(llm.ctx, cPrompt, unsafe.SliceData(cTokens), C.int(len(cTokens)), true); n > 0 {
		tokens := make([]int, n)
		for i := range cTokens[:n] {
			tokens[i] = int(cTokens[i])
		}

		return tokens
	}

	return nil
}

func (llm *llama) Decode(tokens ...int) string {
	var sb strings.Builder
	for _, token := range tokens {
		sb.WriteString(C.GoString(C.llama_token_to_str(llm.ctx, C.llama_token(token))))
	}

	return sb.String()
}

func (llm *llama) next() (C.llama_token, error) {
	llm.mu.Lock()
	defer llm.mu.Unlock()

	if len(llm.embd) >= llm.NumCtx {
		numLeft := (llm.NumCtx - llm.NumKeep) / 2
		truncated := llm.embd[:llm.NumKeep]
		truncated = append(truncated, llm.embd[len(llm.embd)-numLeft:]...)

		llm.embd = truncated
		llm.cursor = llm.NumKeep
		log.Printf("input truncated: num_ctx=%d num_keep=%d num_left=%d num_tokens=%d cursor=%d", llm.NumCtx, llm.NumKeep, numLeft, len(truncated), llm.cursor)
	}

	for {
		if llm.gc {
			return 0, io.EOF
		}

		if llm.cursor >= len(llm.embd) {
			break
		}

		numEval := len(llm.embd) - llm.cursor
		if numEval > llm.NumBatch {
			numEval = llm.NumBatch
		}

		if retval := C.llama_eval(llm.ctx, unsafe.SliceData(llm.embd[llm.cursor:]), C.int(numEval), C.int(llm.cursor), C.int(llm.NumThread)); retval != 0 {
			return 0, fmt.Errorf("llama_eval: %d", retval)
		}

		llm.cursor += numEval
	}

	var sampleOpts C.struct_llama_sample_options
	sampleOpts.repeat_penalty = C.float(llm.RepeatPenalty)
	sampleOpts.frequency_penalty = C.float(llm.FrequencyPenalty)
	sampleOpts.presence_penalty = C.float(llm.PresencePenalty)
	sampleOpts.temperature = C.float(llm.Temperature)
	sampleOpts.top_k = C.int(llm.TopK)
	sampleOpts.top_p = C.float(llm.TopP)
	sampleOpts.tfs_z = C.float(llm.TFSZ)
	sampleOpts.typical_p = C.float(llm.TypicalP)
	sampleOpts.mirostat = C.int(llm.Mirostat)
	sampleOpts.mirostat_tau = C.float(llm.MirostatTau)
	sampleOpts.mirostat_eta = C.float(llm.MirostatEta)
	sampleOpts.penalize_newline = C.bool(llm.PenalizeNewline)

	numVocab := C.llama_n_vocab(llm.ctx)
	logits := unsafe.Slice(C.llama_get_logits(llm.ctx), numVocab)

	// TODO: logit bias

	candidates := make([]C.llama_token_data, numVocab)
	for i := range logits {
		candidates[i] = C.llama_token_data{
			id:    C.int(i),
			logit: logits[i],
			p:     0,
		}
	}

	repeatLastN := llm.RepeatLastN
	if len(llm.last) < repeatLastN {
		repeatLastN = len(llm.last)
	}

	if llm.NumCtx < repeatLastN {
		repeatLastN = llm.NumCtx
	}

	lastN := llm.last[len(llm.last)-repeatLastN:]

	token := C.llama_sample(
		llm.ctx,
		unsafe.SliceData(candidates), C.size_t(len(candidates)),
		unsafe.SliceData(lastN), C.size_t(len(lastN)),
		&sampleOpts,
	)

	llm.last = append(llm.last, token)
	llm.embd = append(llm.embd, token)

	if token == C.llama_token_eos() {
		return 0, io.EOF
	}

	return token, nil
}

func (llm *llama) Embedding(input string) ([]float64, error) {
	if !llm.EmbeddingOnly {
		return nil, errors.New("llama: embedding not enabled")
	}

	tokens := llm.Encode(input)
	if tokens == nil {
		return nil, errors.New("llama: tokenize embedding")
	}

	cTokens := make([]C.llama_token, len(tokens))
	for i := range tokens {
		cTokens[i] = C.llama_token(tokens[i])
	}

	retval := C.llama_eval(llm.ctx, unsafe.SliceData(cTokens), C.int(len(tokens)), 0, C.int(llm.NumThread))
	if retval != 0 {
		return nil, errors.New("llama: eval")
	}

	C.llama_print_timings(llm.ctx)

	n := C.llama_n_embd(llm.ctx)
	if n <= 0 {
		return nil, errors.New("llama: no embeddings generated")
	}
	cEmbeddings := unsafe.Slice(C.llama_get_embeddings(llm.ctx), n)

	embeddings := make([]float64, len(cEmbeddings))
	for i, v := range cEmbeddings {
		embeddings[i] = float64(v)
	}
	return embeddings, nil
}
