# 두 번째 일급 백엔드로서의 SGLang

> `sgl-project/sglang` 커밋 `d9cf7b0`(main, 2026-07-28)의 소스에서 읽어낸 통합 명세.
> 아래 모든 동작 주장은 추론이 아니라 그 트리에서 직접 확인한 것이다. 릴리스가 아니라 커밋을
> 인용하는 이유는 체크아웃이 shallow이고 태그가 없기 때문이다.
>
> [VLLM.ko.md](VLLM.ko.md)의 동반 문서이며, 행 단위로 나란히 읽도록 구성했다. SGLang이 vLLM과
> 같게 동작하는 곳은 그렇다고만 적고 넘어간다. 가치는 §7에 있다.
>
> English (기준 문서): [SGLANG.md](SGLANG.md)

SGLang은 두 가지 면에서 dorang의 목표 표면에 vLLM보다 *가깝다* — Anthropic을 네이티브로 말하고,
load 엔드포인트가 플래그로 막힌 0이 아니라 진짜다. 그리고 더 중요한 두 가지 면에서 *멀다* —
priority가 반대 방향으로 돌고, OpenAI 에러 봉투가 OpenAI의 것이 아니다.

두 엔진 모두 같은 지점들에서 조용히 실패한다. 다만 **서로 다른** 지점들에서 조용히 실패하며,
게이트웨이 입장에서는 그것이 어느 한쪽만 있는 것보다 나쁘다.

---

## 0. dorang 설계를 바꾼 네 가지 발견

### 0.1 priority 방향이 반대이고, dorang의 클래스 맵은 SGLang에 정확히 뒤집혀 있다

vLLM은 **낮은 값 우선**. SGLang은 기본적으로 **높은 값 우선**. 같은 필드 이름, 같은 타입, 반대 의미,
어느 쪽이든 에러 없음.

`--schedule-low-priority-values-first`의 기본값이 `False`이므로 `priority_sign = -1`이 되고,
`-priority`에 대한 오름차순 정렬이 **가장 큰** 값을 앞에 놓는다. 도움말 텍스트도 독립적으로 같은 말을
한다 — *"기본적으로 priority 정수값이 더 높은 요청이 먼저 스케줄된다."*

설계 §7.5의 맵 `{realtime: 0, interactive: 2, batch: 10}`은 vLLM에 대해 옳고 기본 설정 SGLang에 대해
**정확히 뒤집혀 있다**: batch 작업이 realtime을 앞지른다. 두 엔진이 같은 정수를 받고 200을 반환하며
정반대 순서를 만들기 때문에, 이 문서에서 심각도가 가장 높은 항목이다.

빠져나갈 길은 둘이고 dorang은 둘 다 해야 한다:

- **프로바이더별 `priority_direction`** 을 오퍼레이터가 선언(vLLM은 `lower_first`, SGLang은
  `higher_first`)하고, emit 시점에 클래스 맵을 뒤집는다.
- SGLang 런북(§8)에서 **`--schedule-low-priority-values-first`를 권장**한다. 그러면 SGLang이 vLLM과
  일치하고 하나의 맵이 둘을 모두 서빙한다.

### 0.2 컨텍스트 윈도우는 인증 없는 GET 한 번으로 읽힌다 — 같은 호출, 같은 필드

```
GET {base_url}/v1/models  →  data[i].max_model_len
```

`tokenizer_manager.model_config.context_len`, 즉 override 이후의 실효값에서 설정된다. 라우트는 무조건
등록된다.

**이것은 VLLM.md §1.1이 세운 계약과 비트 단위로 같다**, LoRA 규칙까지 포함해서: 어댑터 카드는
`max_model_len=None`과 기반 모델을 가리키는 `parent`를 갖는다. dorang의 기존 `/v1/models` 프로브가
분기 없이 두 엔진 모두에 통한다. 이것은 진짜로 좋은 결과이고 그대로 적어 둘 가치가 있다.

차이는 *그 밖에* 무엇이 있느냐이고 §2에서 다룬다. 중요한 것 하나: SGLang은 `/server_info`를 추가로
노출하며, 이것은 전체 `ServerArgs` 데이터클래스를 — §8이 말하는 모든 플래그를 런타임에 읽을 수 있게 —
반환한다. 그리고 서버 자신의 API 키까지 **가리지 않고** 반환한다.

### 0.3 `/v1/messages`는 네이티브로 서빙되며, passthrough가 아니라 adapter다

`POST /v1/messages`와 `POST /v1/messages/count_tokens`가 등록된다. 어느 쪽도 플래그로 막히지 않는다.
요청/응답 모델은 진짜 Anthropic 구현(518줄)이고, 서빙 레이어(1452줄)가
Anthropic → `ChatCompletionRequest` → OpenAI chat 파이프라인 → 다시 Anthropic 이벤트로 변환한다.

dorang에게 이것은 체크박스가 아니라 실제 능력이다: Anthropic 형태 요청을 번역하지 않고 전달할 수 있다.
그러나 **COMPATIBILITY 6.4가 이미 명세한 것과 같은 손실성 붕괴**를 가진 adapter이므로, dorang은 진실을
out-of-band로 보고할 같은 의무를 상속한다:

```
STOP_REASON_MAP = {"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use"}
```
그 밖의 모든 것 — `abort` 포함 — 은 WARNING과 함께 `end_turn`으로 떨어진다.

### 0.4 OpenAI 에러 봉투가 중첩되어 있지 않고, 한 서버에 다섯 가지 형태가 있다

```python
class ErrorResponse(BaseModel):
    object: str = "error"
    message: str
    type: str
    param: Optional[str] = None
    code: int
```

`serving_base.py`가 `error.model_dump()`를 **평평하게** 반환한다. OpenAI SDK는
`body["error"]["message"]`를 읽는데, SGLang에는 그 키가 없다. vLLM은 최소한 중첩은 한다. 이것은
VLLM.md §2.2 같은 "code 필드 정규화" 문제가 아니라 *구조적* 문제이고, 수정되지 않은 OpenAI SDK는
메시지를 노출하는 대신 파싱에서 예외를 던진다.

같은 배포에서 도달 가능한 다섯 형태는 §6.2에 열거한다.

---

## 1. HTTP 표면

### 1.1 등록된 라우트

**OpenAI 호환**: `POST /v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/classify`,
`/v1/tokenize`(+`/tokenize`), `/v1/detokenize`(+`/detokenize`), `/v1/score`, `/v1/rerank`(PUT도),
`/v1/responses`, `GET /v1/responses/{id}`, `POST /v1/responses/{id}/cancel`, `GET /v1/models`,
`GET /v1/models/{model:path}`, `POST /v1/audio/transcriptions`, `WS /v1/realtime`(transcription 서브셋만).
모두 게이트 없음.

이름만 OpenAI 형태이고, **필드 이름 외 세 가지 — 추가 필드, 에러 봉투, 스트리밍 청크 내용 — 에서는
OpenAI 형태가 아니다.** 전부 §6.

**Anthropic 호환**: `POST /v1/messages`, `POST /v1/messages/count_tokens`. 게이트 없음.

**Ollama 호환**: `POST /api/chat`, `/api/generate`, `GET /api/tags`, `POST /api/show`. 각 경로는
import 시점에 환경변수에서 읽히므로 **라우트 경로 자체가 오퍼레이터 변경 가능**하다. `GET|HEAD /`는
리터럴 문자열 `"SGLang is running"`을 반환한다(환경변수로 `"Ollama is running"`).

**벤더**: SageMaker용 `GET /ping`, `POST /invocations`(= 다른 이름의 `/v1/chat/completions`),
`POST /vertex_generate`(경로 환경변수로 override 가능).

**네이티브**: `POST|PUT /generate`, `/encode`, `/classify`.

**Health와 info**: `GET /health`, `/health_generate`, `/model_info`(+deprecated `/get_model_info`),
`/server_info`(+deprecated `/get_server_info`), `/v1/loads`(+deprecated `/get_load`).
`GET /get_weight_version`과 `/weight_version`은 **deprecation 메시지와 함께 404를 반환하기 위해서만
존재한다** — 이들을 프로브하는 게이트웨이는 "없음"이 아니라 "옮겨짐"을 뜻하는 404를 본다.

### 1.2 `/v1/messages`가 실제로 말하는 것

dorang이 전달할지 번역할지를 결정하는 절.

**내보내는 이벤트**: `message_start`, `content_block_start`, `content_block_delta`,
`content_block_stop`, `message_delta`, `message_stop`. 프레이밍은 `event: <type>\ndata: <json>\n\n`
— 두 줄, COMPATIBILITY 6.1과 일치. **`[DONE]` 없고 `ping` 없음**: `PingEvent`는 선언돼 있으나 어디서도
생성되지 않는다. `error` 이벤트는 존재하고 업스트림 실패 시 스트림 도중 방출된다. 컨텐트 블록 타입은
`text`, `thinking`, `tool_use`, 그리고 백엔드가 thinking signature를 주면 `signature_delta`.

**`stop_reason` 값**: 와이어 타입은 네 값 `Literal`(`end_turn`, `max_tokens`, `stop_sequence`,
`tool_use`)이지만 `STOP_REASON_MAP`은 그중 셋만 만들 수 있다 — **`stop_sequence`는 선언되고 절대
방출되지 않는다.** adapter가 OpenAI 레이어의 `matched_stop`을 읽지 않기 때문이다. 즉 SGLang은 정보를
가지고 있으면서(§6.4) 이 경로에서 버린다. `refusal`, `pause_turn`,
`model_context_window_exceeded`는 존재하지 않는다.

**`stop_sequence`는 null이 아니라 생략된다.** `model_dump(exclude_none=True)`로 직렬화하므로 키가
아예 없다. Anthropic 자체 API는 `"stop_sequence": null`을 내보낸다. COMPATIBILITY 6.5는 adapter
경로에서 이 필드가 null이라고 하므로, SGLang 상대로는 dorang이 키를 합성해야 한다.

**Usage**: `input_tokens`는 캐시 read를 **제외**한 값(`max(prompt - cached, 0)`)으로, Anthropic 자신의
관례이자 설계 §10.7의 정규화 대상과 일치한다. `cache_read_input_tokens`는 0이 아닐 때만 나온다.
**`cache_creation_input_tokens`는 어디에서도 설정되지 않는다** — 필드는 선언돼 있고 writer가 없다.
`total_tokens`가 **없으므로** SGLang은 COMPATIBILITY 6.8의 비대칭을 재현하지 *않는다*;
`compat.anthropic_total_tokens`가 켜져 있으면 dorang이 추가해야 한다.

**받아들이고 조용히 버리는 것**: `metadata`가 선언돼 있고 절대 읽히지 않는다 — 따라서
`metadata.user_id`를 통한 `EndUser`가 유실된다. 컨텐트 블록의 `cache_control`은 모델에 아예 없다:
`TextBlock`은 `{type, text}`이고 Pydantic 기본 `extra='ignore'`가 버린다. **Anthropic 프롬프트 캐싱
breakpoint는 표현 불가능하다.**

**받아들이고 로그하고 강제하지 않는 것**: `thinking.budget_tokens`, `thinking.display: "omitted"`,
`betas`, `output_config.task_budget`. Anthropic 서버 툴(`web_search_*`, `computer_*`, `bash_*`,
`text_editor_*`)은 파싱되고 인식되고 INFO 로그와 함께 **건너뛴다** — 즉 그것들을 쓰는 요청은 툴을
조용히 버린 채 200을 반환한다.

**존중되는 것**: `system`(문자열 또는 블록 리스트), `temperature`, `top_p`, `top_k`,
`stop_sequences` → `stop`, `tools` → OpenAI tools, `tool_choice`, `thinking.type != "disabled"` →
리즈닝 on, `output_config.effort` → `reasoning_effort`(`xhigh → max`). `max_tokens`는 필수이며 양수
검증된다. 스트리밍은 `stream_options.include_usage=True`를 강제하므로 usage는 항상 도착한다.

**`priority` 없음.** `AnthropicMessagesRequest`에 그 필드가 없고, 변환은 명시적 화이트리스트로
`ChatCompletionRequest`를 만들며 거기에 절대 포함되지 않는다. VLLM.md §1.2와 같은 결론: priority가
필요한 Anthropic 트래픽은 `/v1/chat/completions`로 번역해야 한다.

**에러는 올바르게 Anthropic 형태다** — `{"type":"error","error":{"type","message"}}`, 문서화된
status→type 맵과 5xx 메시지 스크러빙 포함. 이 서버에서 stock SDK가 제대로 파싱하는 유일한 에러 표면이다.

**`count_tokens`는 정확히 `{"input_tokens": N}`을 반환**한다. `?beta=true` 쿼리 파라미터는 받지 않지만
FastAPI가 여분 쿼리를 무시하므로 무해하다.

### 1.3 batch API는 없다 — 라우트 등록으로 확정

`/v1/batches`는 **존재하지 않는다.** batch job 클래스도, 폴링 라우트도, 오프라인 batch CLI도 이 트리에
없다 — SGLang에는 vLLM의 `run_batch` 스크립트조차 없다.

vLLM의 `/v1/chat/completions/batch`에 해당하는 동기식 팬아웃도 없다. 가장 가까운 것은 `/generate`와
`/encode`가 리스트 입력을 받아 내부적으로 팬아웃하는 것인데, 그것은 네이티브 API이지 OpenAI batch
형태가 아니며 여전히 하나의 all-or-nothing HTTP 요청이다.

**batch 스케줄러가 dorang 자신의 것이라는 설계 §11.1의 결정은 두 엔진 모두에 대해 옳음이 확인됐다.**
SGLang은 부분 점수조차 주지 않는다.

### 1.4 개발/관리 라우트와 실제로 그것들을 막는 것

SGLang의 관리 표면은 vLLM보다 훨씬 크고, 게이트는 보이는 것보다 약하다.

**파괴적이거나 상태를 바꾸며 `ADMIN_OPTIONAL`로 표시된 것**: `/flush_cache`, `/set_internal_state`,
`/add_external_corpus`, `/remove_external_corpus`, `/list_external_corpora`, 여섯 개의
`/hicache/storage-backend*`, `/start_profile` `/stop_profile` `/set_trace_level` `/freeze_gc`,
expert-distribution 라우트 셋, weight-update 라우트 열넷, `/get_weights_by_name`,
`/release_memory_occupation` `/resume_memory_occupation`, `/weights_checker`, `/slow_down`,
LoRA 라우트 셋, `/open_session` `/close_session`, `/configure_logging`, `/abort_request`,
`/pause_generation` `/continue_generation`, `/scale_elastic_ep` `/is_scaling_elastic_ep`.

**`ADMIN_OPTIONAL`은 "키가 설정되지 않았으면 열림"을 뜻한다.** `--api-key`도 `--admin-api-key`도
없으면 `AuthDecision(allowed=True)`다. 즉 기본 실행 서버에서는 *위 목록의 모든 라우트가 인증 없이*
열려 있다 — `/flush_cache`, `/slow_down`, `/pause_generation`, weight-update 계열 포함.

오퍼레이터가 반드시 알아야 할 세 가지:

- **`--api-key`는 single-tokenizer 모드에서만 연결된다.** `tokenizer_worker_num == 1` 분기 안에서만
  미들웨어가 추가되고, `else` 분기는 인증을 전혀 추가하지 않는다. **`--tokenizer-worker-num 2
  --api-key secret`으로 띄운 서버는 완전히 인증 없이 돌며 아무 에러도 보고하지 않는다.**
- **`/health*`와 `/metrics`는 키가 설정돼 있어도 프리픽스로 항상 허용된다.** k8s와 Prometheus를 위한
  의도적 설계이고, 그 결과 잠긴 서버에서도 `/health_generate` 프로브가 진짜 1토큰 생성을 돌린다.
- **CORS가 `allow_origins=["*"]` + `allow_credentials=True`** 로 모든 라우트에 무조건 걸린다.

**dorang은 이들 중 무엇도 프로덕션 백엔드에 호출해서는 안 된다.** VLLM.md §5와 같은 결론이며, 뒤에
더 큰 표면이 있다: `/flush_cache`는 radix 캐시를 버리고, `/slow_down`은 스케줄러를 조이며,
`/pause_generation`은 멈춘다.

---

## 2. 컨텍스트와 모델 메타데이터

### 2.1 `GET /v1/models`

`ModelCard`는 `id`, `object`, `created`, `owned_by`, `root`, `parent`, `max_model_len`을 갖고
`ModelList{object:"list", data:[...]}`로 감싸진다.

- **`max_model_len`이 실효 컨텍스트 윈도우를 담는다** — `model_config.context_len`에서.
- `id`와 `root`는 `served_model_name`이며 기본값은 `model_path`.
- `--enable-lora`일 때 LoRA 어댑터가 추가되고, `parent`는 기반 모델, `max_model_len=None` — vLLM 규칙과
  동일.
- **`created`는 응답마다 평가되는 `int(time.time())`** 이지 고정 상수가 아니다. 호출할 때마다 바뀐다.
  COMPATIBILITY 7.4는 상수를 요구하지만 dorang이 어차피 이 필드를 다시 찍으므로 결함이 아니라 메모다.

`GET /v1/models/{model}`은 감싸지 않은 `ModelCard`를 반환하고 이름 불일치 시 **중첩·문자열 code**
에러 봉투로 404를 낸다 — 이 서버의 다른 어떤 에러도 쓰지 않는 형태다.

### 2.2 `/model_info`에는 컨텍스트 길이가 없다

`{"model_path", "tokenizer_path", "is_generation", "preferred_sampling_params",
"weight_version", "has_image_understanding", "has_audio_understanding", "model_type",
"architectures"}`. **컨텍스트 길이 필드가 없다.** 이름이 가장 그럴듯한 엔드포인트로 손을 뻗은
게이트웨이는 원하던 그 숫자 하나만 빼고 전부를 얻는다.

### 2.3 `/server_info` — 전부, 키까지 포함해서

전체 `ServerArgs` 데이터클래스를 반환한다. §8이 묻는 모든 질문에 completion 요청 없이 답한다 —
해결된 `context_length`, `schedule_policy`, `enable_priority_scheduling`,
`schedule_low_priority_values_first`, `page_size`, `enable_cache_report`, `tool_call_parser`,
`reasoning_parser`, `allow_auto_truncate`, `max_running_requests`. VLLM.md §1.2가 쓸 수 없었던 vLLM
개발 모드 엔드포인트의 SGLang 대응물인데, 무조건 등록되고 `ADMIN_OPTIONAL` 표시조차 없다.

**그리고 가려지지 않는다.** `api_key`와 `admin_api_key`는 평범한 데이터클래스 필드이고 `asdict`가
포함하며, 호출 경로 어디에도 redaction이 없다. 결과 둘:

- `--admin-api-key`만 설정하면 `NORMAL` 레벨 라우트가 열려 있으므로 `/server_info`가 **아무 호출자에게나
  admin 키를 건넨다.**
- `--tokenizer-worker-num > 1`(§1.4)에서는 어떤 키를 설정했든 마찬가지다.

dorang은 신뢰된 네트워크 배포에서 capability discovery를 위해 `/server_info`를 읽을 수 있지만, 반드시
**프로바이더별 명시 옵션**이어야 하고, 절대 로그되어서는 안 되며, 가용성을 가정해서도 안 된다.

### 2.4 컨텍스트 길이 도출과 override

`--context-length`는 **좁히기만** 한다. 모델 자체 도출값보다 넓히는 것은
`SGLANG_ALLOW_OVERWRITE_LONGER_CONTEXT_LEN=1`(기본 False)이 아니면 **기동 에러**다. vLLM은 경고하고,
SGLang은 거부한다. 더 안전한 동작이며 오퍼레이터 프로필에 적어 둘 가치가 있다.

### 2.5 실무적 답

**그렇다. 인증 없는 `GET /v1/models` 한 번, 필드 `data[i].max_model_len`.** vLLM과 동일한 호출,
동일한 필드. 컨텍스트 discovery에 엔진별 분기가 필요 없다. `null`은 미선언(LoRA 카드)이고,
**읽을 max-output 값이 없으며 dorang이 합성해서도 안 된다** — 둘 다 vLLM과 같다.

---

## 3. 스케줄링 priority

### 3.1 필드

`priority: Optional[int] = None`이 `CompletionRequest`, `ChatCompletionRequest`, `EmbeddingRequest`,
`ClassifyRequest`에 있다. `ResponsesRequest`에서는 `priority: int = Field(default=0, ...)`로
non-optional + 기본값이다.

**`/v1/responses`는 선언만 하고 연결하지 않는다.** `GenerateReqInput` 생성 어디에서도 `priority`를
넘기지 않는다. 값은 지역 변수로 읽혀 내장 툴 왕복마다 1씩 감소되고 버려진다. 따라서 vLLM §1.2의
"responses가 priority를 변형한다" 위험은 여기 **해당하지 않는다** — 필드가 그냥 죽어 있다. 실패는
다르고, 실무 조언은 같다: priority를 실은 트래픽을 `/v1/responses`로 보내지 말 것.

### 3.2 방향, 세 가지로 확인

§0.1에서 확립. 독립 확증 셋: `priority_sign` 계산, `(priority * priority_sign, wait_queue_entry_time)`
오름차순 정렬, 그리고 higher-first가 기본이라고 말하는 도움말 텍스트. 네 번째 결정적 증거: priority가
없을 때 채우는 기본값이 low-values-first면 `sys.maxsize`, 아니면 `-sys.maxsize - 1` — 두 경우 모두
*최악* 값이다. priority가 없는 요청은 priority가 있는 요청들 사이에서 항상 마지막에 스케줄된다.

### 3.3 정책, 그리고 크래시하는 하나

선택지는 `lpm`, `random`, `fcfs`, `dfs-weight`, `lof`, `priority`, `routing-key`. **기본 `fcfs`.**

**`priority`는 구현이 없는 광고된 선택지다.** 두 enum 중 어느 쪽에도 매칭되지 않아
`raise ValueError(f"Unknown schedule_policy: {policy=}")`로 떨어진다. vLLM의 자기부정적 필드 설명에
대응하는 SGLang의 사례다: CLI가 동작하지 않는 것을 광고한다. 런타임에 조용히 실패하는 대신 기동에서
크게 실패하니 더 나은 실패이긴 하나, **dorang 설정 검증기는 이 값을 제안해서는 안 된다.**

조용한 다운그레이드 둘: 대기 큐가 128을 넘으면 `lpm`이 `fcfs`로, radix 캐시가 꺼져 있으면 모든
cache-aware 정책이 `fcfs`로 떨어진다.

### 3.4 기본값에서 priority가 조용히 무시되는가? 그렇다 — 다만 SGLang은 말하게 할 수 있다

`--enable-priority-scheduling`의 기본값은 `False`다. 꺼져 있으면: 프로토콜 레이어에 `priority` 검증기가
없고, 기본값 채우기는 *채우기만* 하며 제거하거나 거부하지 않고, 값은 스케줄러로 전달되어 `Req`에
저장되며, 정렬은 `if self.enable_priority_scheduling` 뒤에 있으므로 큐를 건드리지 않고 반환한다.

즉 기본값은 **200 OK, 경고 없음, 효과 없음** — vLLM §1.2와 같은 모양이고 응답으로 탐지 불가능하다.

**그러나 SGLang에는 vLLM에 대응물이 없는 opt-in 거부가 있다:**
`--abort-on-priority-when-disabled`(기본 `False`)는 조용한 drop을 **안정적 메시지 부분 문자열을 가진
503**으로 바꾼다. 그래서 SGLang의 priority 지원은 *프로브 가능*하다: dorang이 priority를 실은 버리는
요청 하나를 보내 오퍼레이터 선언이 참인지 응답에서 배울 수 있다.

그 프로브는 오퍼레이터가 그 플래그를 켰을 때만 유효하므로, §8의 권장은 **둘 다** 켜는 것이다 —
앞의 것이 priority를 동작하게 하고, 뒤의 것이 오설정을 시끄럽게 만든다.

priority 스케줄링은 `schedule_policy in {fcfs, lof}`를 **하드 assert**한다. `lpm`, `dfs-weight`,
`random`, `routing-key`에서는 효과가 없고, 서버는 degrade 대신 기동을 거부한다.

### 3.5 선점 — SGLang에는 있고 vLLM에는 없다

VLLM.md §1.2는 "priority는 선점을 의미하지 않는다"로 끝난다. **SGLang에서는 의미하며**, priority
스케줄링이 켜지면 기본으로 함께 켜진다.

실행 중 요청을 덜 중요한 순으로 정렬하고, priority 차이가
`--priority-scheduling-preemption-threshold`(기본 **10**)를 넘는 것들을 선점한다. 선점은
all-or-nothing이다: 희생자들이 충분한 토큰을 못 비우면 아무것도 선점되지 않는다.

**선점된 요청은 abort되지 않고 재큐잉된다.** 큐로 돌아가 `output_ids`를 유지한 채 재개하므로
클라이언트는 에러가 아니라 지연을 본다. 비용 둘: KV prefix가 radix 트리에 **재삽입되지 않으므로**
매 라운드가 전체 재계산이고, `retraction_count`가 증가하지만 상한이 없어 고우선 트래픽이 꾸준히
들어오면 저우선 요청을 무한정 굶길 수 있다.

**설계 §7.5 클래스 맵에 대한 결과.** 기본 임계값 10과 dorang의 간격 `{0, 2, 10}`으로는 아무것도 바를
넘지 못한다(차이 10은 `> 10`이 아니다). 밴드를 넓히거나 임계값을 낮춰야 한다 — §8 런북은 방향을 뒤집은
밴드 `{0, 5, 10}`과 `--priority-scheduling-preemption-threshold 5`를 권장해, 정확히 한 단계의 선점이
가능하게 한다.

**priority가 실제로 abort시키는 경우 하나.** `--max-queued-requests`에 도달했을 때, 대기 중 가장 낮은
priority 요청이 `type: "abort"`, HTTP 503,
`"The request is aborted by a higher priority request."` 메시지로 축출된다. priority 스케줄링이 없으면
들어오는 요청이 `"The request queue is full."`로 거부된다. `--max-queued-requests`의 기본값은 `None`,
즉 무제한이며, disaggregation 모드에서는 **완전히 무시된다.**

### 3.6 게이트웨이가 보낼 수 있는 그 밖의 스케줄링 영향 필드

| 필드 | 효과 |
|---|---|
| `lora_path` | 하드 admission 게이트 — 어댑터가 `max_loras_per_batch`를 넘길 요청은 건너뛴다. **VLLM.md §6과 같은 "priority보다 앞" 규칙** |
| `routed_dp_rank` | DP 워커 고정. 범위 검증 후 400 — 잘못된 값을 무시하는 vLLM의 `X-data-parallel-rank`와 다르다 |
| `routing_key` | `--schedule-policy routing-key`에서 큐를 직접 재정렬. `X-SMG-Routing-Key` 헤더로도 읽힌다 |
| `extra_key` / `cache_salt` | radix 네임스페이스 — §5 |
| `session_id`, `session_params` | 세션 태그 KV; 상호 배타 |
| `bootstrap_host/port/room` | PD-disaggregation 타깃팅. 클라이언트가 **서버가 접속할** 호스트와 포트를 제공한다 — 특권으로 취급할 것 |
| `sampling_params.max_new_tokens` | `lof`의 보조 정렬 키이자 선점 토큰 계산의 입력 |

**`rid`는 OpenAI 라우트에서 선언되고 죽어 있다.** `_generate_request_id_base`가 `request.rid`를 읽을
블록 앞에서 `return None`을 실행하고, 어떤 서브클래스도 override하지 않는다. 따라서 `X-Request-Id`를
무조건 존중해 엔진 요청 id로 채택하는 vLLM과 달리 **SGLang은 OpenAI 표면에서 dorang에게 엔진 로그로의
상관관계 수단을 전혀 주지 않는다.**

`x-override-rid` / `x-override-priority` 헤더 계열은 네이티브 `/generate`에만, 그리고
`SGLANG_ENABLE_REQUEST_HEADER_OVERRIDES=1`(기본 False)일 때만 적용된다. 설계 §7.5의
`X-Request-Priority` 헤더는 여기서 무력하다.

### 3.7 요청 타임아웃

요청별 deadline 필드는 없다. 기본 비활성인 전역 환경변수 둘: `SGLANG_REQ_WAITING_TIMEOUT`(큐잉된 요청을
503 `"Request waiting timeout reached."`로 abort)과 `SGLANG_REQ_RUNNING_TIMEOUT`(실행 중 요청을 503
`"Request running timeout reached."`로 abort).

---

## 4. 메트릭

### 4.1 게이트, 그리고 그것이 vLLM보다 나은 이유

`--enable-metrics`가 있을 때만 `/metrics` 마운트가 추가된다. 그것이 HTTP 서버의 유일한 `/metrics`
등록이고 catch-all 라우트는 없다.

**`--enable-metrics` 없이는 `GET /metrics`가 404다.** 이것은 vLLM의 함정(VLLM.md §3.1 마지막 항목)보다
엄격히 낫다 — 거기서는 `--disable-log-stats`가 시리즈 0개로 200을 반환해 "도달 불가"와 "유휴"를
구별할 수 없다. dorang은 상태 코드만으로 두 경우를 구별할 수 있다.

`--enable-metrics`의 기본값은 `False`이므로 기본 실행 SGLang에는 메트릭이 전혀 없다.

### 4.2 스크레이프할 가치가 있는 메트릭

이름은 `sglang:` 프리픽스. 스케줄러 메트릭은 `{model_name, engine_type, tp_rank, pp_rank,
moe_ep_rank}`에 DP가 켜지면 `dp_rank`, priority 스케줄링이 켜지면 `priority`가 추가된다. Tokenizer
메트릭은 `{model_name, engine_type}`만. **`engine` 라벨은 없다** — SGLang의 대응물은 `engine_type`이며
그 값은 DP rank가 아니라 disaggregation 역할이다.

| 신호 | 메트릭 |
|---|---|
| 실행 중 요청 | `sglang:num_running_reqs` |
| 대기 요청 | `sglang:num_queue_reqs` (+ `sglang:num_grammar_queue_reqs`) |
| KV 사용률 | `sglang:full_token_usage` — **`sglang:token_usage`보다 이것을 쓸 것** |
| TTFT | `sglang:time_to_first_token_seconds` |
| prefix 캐시 | `sglang:cached_tokens_total{cache_source}` — **토큰 단위**, `sglang:prompt_tokens_total`과 함께 |
| preemption | `sglang:num_retracted_requests_total` |
| 처리량 | `sglang:gen_throughput` (token/s) |
| HTTP | `sglang:http_requests_total`, `sglang:http_responses_total{status_code}` |
| 상수 | `sglang:context_len`, `sglang:page_size`, `sglang:max_total_num_tokens` |

### 4.3 함정

VLLM.md §3.1은 넷을 열거한다. SGLang은 더 많고, 여럿은 단지 잘못 명명된 게 아니라 **죽었거나 리셋된다**
는 점에서 더 나쁘다.

- **`sglang:cache_hit_rate`는 매 decode 리포트마다 `0.0`으로 하드 리셋된다.** prefill 리포트가 실제
  값을 넣지만 decode 리포트는 `--decode-log-interval`(기본 40) 반복마다 발화하고 prefill 리포트는 extend
  배치에서만 발화한다. **게이지는 대부분의 시간을 0에서 보내며**, 그것에 대한 `avg_over_time`은 적중률이
  아니라 prefill/decode 리포트 비율을 잰다. 이 표면에서 가장 오해를 부르는 시리즈이고, VLLM.md §3.3이
  `/load`에 대해 지적하는 "매력적인 기본값" 실패 모드와 정확히 같다 — 다만 여기서는 그것이 *유일하게*
  이름 붙은 적중률 메트릭이다. **`sglang:cached_tokens_total`과 `sglang:prompt_tokens_total`에서
  계산할 것.**
- **`sglang:is_cuda_graph`는 죽어 있다.** 어디에서도 할당되지 않아 영구 `0`, 즉 "CUDA graph를 쓴 적
  없음". `sglang:cuda_graph_passes_total{mode}`를 쓸 것.
- **`sglang:utilization`은 `0`에 고정되거나 문자 그대로 `-1`이다.** 입력에 setter가 없고(소스에 TODO가
  달려 있다), PD-prefill 모드에서는 `-1`이 할당된다. 음수 utilization은 어떤 순진한 임계값 검사도
  통과한다.
- **`sglang:engine_startup_time`과 `sglang:engine_load_weights_time`은 유일한 호출 지점에서 `0.0`으로
  하드코딩**돼 있다.
- **`sglang:num_retracted_reqs`와 `sglang:num_paused_reqs`는 델타를 담은 게이지다.** 매 publish 후
  0으로 리셋된다. 문서 문자열은 "retract된 요청의 수"라고 말한다. `_total` 카운터를 쓸 것.
- **`sglang:token_usage`는 소수점 2자리로 반올림**되고(1% 정밀도) KV 풀이 아니라
  `max(full, swa, mamba)`다. vLLM의 것처럼 0–1 분수이고, vLLM의 것처럼 이름이 그렇게 말하지 않는다.
  소스 자체에 `FIXME: misleadingly named "token_usage"`가 달려 있다.
- **`sglang:inter_token_latency_seconds`는 `_sum`/`_count` 스케일이 어긋난다.** `_sum`은 원시 간격만큼
  한 번 증가하고 버킷은 `num_new_tokens`만큼 증가한다. 정규 평균인 `rate(_sum)/rate(_count)`가 틀린다.
  더 나쁘게, 최상위 버킷 경계를 넘는 관측은 어떤 버킷과도 매칭되지 않아 완전히 버려지므로 tail이
  버킷과 count 양쪽에서 과소 보고된다.
- **PD-prefill 노드에서는 TTFT가 전혀 기록되지 않는다.** 시리즈가 부재하는 대신 관측 0개로 조용히 남는다.
- **`--enable-metrics-for-all-schedulers`가 없으면 `attn_tp_rank == 0`만 스케줄러 게이지를 내보낸다.**
  그런데 retract 카운터, EPLB, 기동 상수는 그 게이트를 우회하므로 `tp_rank`에 걸친 `sum()`이 retract
  카운트를 TP 크기만큼 곱한다.

**인증 없는 호출자가 도달 가능한 cardinality 위험 둘:**

1. `--tokenizer-metrics-allowed-custom-labels`는 라벨 *키*를 허용목록에 넣지만, **값은 `x-custom-labels`
   헤더에서 오는 임의의 클라이언트 문자열**이며 모든 tokenizer 메트릭에 붙는다.
2. priority 스케줄링이 켜지면 `priority`가 라벨이 되고, 내부 집합이 지금까지 본 모든 정수를 누적해
   스크레이프마다 전부 다시 내보낸다.

dorang이 클라이언트 제어 값을 둘 중 어느 쪽으로든 전달하면, 게이트웨이 기능이 백엔드 메트릭
레지스트리에 대한 서비스 거부로 바뀐다. 하지 말 것.

### 4.4 `/v1/loads` — vLLM의 `/load`가 그랬어야 할 엔드포인트

vLLM 대비 가장 명확한 승리.

`GET /v1/loads`는 **무조건 등록**되고 **플래그가 필요 없다**. DP rank별로 `num_running_reqs`,
`num_waiting_reqs`, `num_waiting_uncached_tokens`, `num_used_tokens`, `num_total_tokens`,
`num_active_tokens`, `max_total_num_tokens`, `max_running_requests`, `token_usage`,
`gen_throughput`, `cache_hit_rate`, `utilization`을 반환하고, `?include=`로 `memory`,
`speculative`, `lora`, `disaggregation`, `queues` 섹션을 고를 수 있다. `?format=prometheus`도 있다.

데이터는 진짜다: 스케줄러가 매 prefill과 idle 전환마다, 그리고
`--load-snapshot-publish-interval`(기본 15) decode 반복마다 공유 메모리 스냅샷을 publish한다.

**VLLM.md §3.3과의 대비가 전부다.** vLLM의 `/load`는 `--enable-server-load-tracking` 없이 영원히 `0`을
반환하고, 설정되지 않은 `0`은 least-busy 라우터에게 가장 매력적인 값이다. SGLang의 것은 플래그가 필요
없고 살아 있는 데이터를 반환한다.

dorang이 코딩해야 할 실패 모드 둘:

- **writer 생성 시점에, 어떤 forward pass보다 먼저 0 스냅샷이 기록된다.** 모든 필드가 `0`이고
  `max_total_num_tokens`도 포함된다. **`max_total_num_tokens == 0`은 "유휴"가 아니라 "준비 안 됨"으로
  취급할 것.**
- **SHM attach 실패는 HTTP 200과 빈 리스트를 반환한다.** `{"loads": []}`는 유휴가 아니라 고장이다.
  최소한 0 스냅샷 경우와는 구별된다.

`/v1/loads?format=prometheus`는 **충돌하는 두 번째 네임스페이스**를 내보낸다: 이름이 `sglang:`이 아니라
`sglang_`이고, 진짜 누적 카운터까지 포함해 모든 필드가 `# TYPE ... gauge`로 선언되며, 라벨은 `dp_rank`
하나뿐이다. 이 형태 말고 JSON 형태를 스크레이프할 것.

### 4.5 응답별 부하 헤더 없음

vLLM의 `endpoint-load-metrics-format: JSON` 기법(VLLM.md §3.2)에 **SGLang 대응물은 없다.**
`entrypoints/` 전체에서 설정되는 응답 헤더는 SSE 배관뿐이다. SGLang에서는 `least_busy`가 `/v1/loads`를
폴링해야 한다 — 값싸고 인증이 필요 없으므로 들리는 것보다 작은 손실이다.

---

## 5. Prefix 캐싱

**RadixAttention은 기본 활성.** `--disable-radix-cache`의 기본값은 `False`. 두 설정이 내부적으로 강제
비활성화한다.

**granularity는 `page_size` 토큰이고 기본값은 1이다.** `--page-size`는 기본 `None`이고 플랫폼과
attention 백엔드별로 해결된다: 표준 CUDA에서 **1**, 백엔드에 따라 16/64/128로 override된다.
`match_prefix`는 매칭 전에 조회 키를 `page_size`의 배수로 잘라낸다.

즉 SGLang은 기본적으로 **토큰 단위**로 매칭하며, vLLM의 기본은 16토큰 블록이다. vLLM과 달리 실효값이
**런타임에 읽힌다** — `sglang:page_size`와 `/server_info`.

### 5.1 dorang의 바이트 경계 prefix chain은 바뀌지 않아야 한다

VLLM.md §4가 준 세 근거를 SGLang에 대조하면:

1. **엔진 경계에 맞추면 핫패스 토큰화를 강제한다.** 동일하게 성립하며, 이제 *더* 명확히 무의미하다:
   기본 granularity가 1토큰이므로 맞출 대상 자체가 없다.
2. **실효 블록 크기가 attention 백엔드에 의해 조용히 재작성된다.** 성립한다 — 위의 override가 바로
   그것이다. 다만 SGLang은 해결된 값을 dev 엔드포인트 뒤에 숨기지 않고 publish하므로 결과는 덜 심각하다.
3. **조회가 prefix-chained이므로 맞춰도 얻는 게 없다.** 성립하며 직접 확인된다: `RadixCache.match_prefix`가
   토큰 id 시퀀스 위로 트리를 걷는다.

**결론: 설계 §7.4b는 두 엔진 모두에 대해 그대로 둔다.** VLLM.md의 핫패스 토큰화 거부가 이제 한 번이
아니라 두 번 확증됐다.

### 5.2 `cache_salt`는 여기에도 있고, 옆에 필드가 하나 더 있다

`cache_salt`와 `extra_key`가 둘 다 `CompletionRequest`와 `ChatCompletionRequest`에 선언돼 있고,
연결되어 `RadixKey.extra_key`로 실려 트리 전체를 네임스페이싱한다 — *"선행 토큰 id가 동일해도
`extra_key`가 다른 엔트리는 의도적으로 분리되며 prefix 노드를 절대 공유하지 않는다."*

**VLLM.md §4와 같은 트레이드오프, 같은 권장.** 설계 §7.4a의 `(tenant, group, session)` 격리에 엔진
내부에서 실질적 힘을 주고, 공유 시스템 프롬프트의 테넌트 간 재사용을 파괴한다. 명시적 프로바이더별
옵션이어야 하고 **기본은 꺼짐** — 그리고 두 엔진이 같은 필드 이름으로 지원하므로 dorang은 한 번만
표현하면 된다.

SGLang 고유 주의 셋, 첫 번째가 무는 것:

- **`cache_salt`는 L3 스토리지 계층을 네임스페이싱하지 않는다.** hierarchical 캐시의 SHA-256 페이지
  해시는 **토큰 id만으로** 계산되며 `extra_key`를 건드리지 않는다. 스토리지 키는 테넌트가 아니라 모델
  이름과 TP/PP rank로 네임스페이싱된다. **`--hicache-storage-backend` 배포에서는 토큰 prefix가 동일한
  두 테넌트가 서로 다른 salt를 써도 L3 블록을 공유한다.** dorang이 `cache_salt`를 격리에 쓴다면, 보장이
  L1/L2에서 성립하고 L3에서 성립하지 *않음*을 기록해야 하며 hicache 스토리지를 공유 테넌시 표면으로
  취급해야 한다.
- **두 필드가 구분자 없이 연결된다.** `cache_salt="a", extra_key="bc"`와 `cache_salt="ab",
  extra_key="c"`가 같은 네임스페이스에 떨어진다. dorang은 둘 중 **정확히 하나만** 채워야 하며 둘 다
  채우면 안 된다.
- **`/v1/messages`로는 `cache_salt`를 설정할 수 없다.** Anthropic adapter가 그것을 뺀 화이트리스트로
  `ChatCompletionRequest`를 만든다.

SGLang이 스스로 `extra_key`에 접어 넣는 것 둘 — LoRA 어댑터 id(따라서 어댑터는 이미 분리된 서브트리를
갖는다)와 elastic-EP 크기 — 는 dorang이 중복해서 넣으면 안 된다.

멀티모달 입력은 흔한 예상과 달리 캐시 *친화적*이다: 이미지·오디오·비디오 placeholder 토큰 id가 컨텐트
해시로 덮어써져 동일한 미디어가 동일한 토큰 id를 만들고 트리에서 매칭된다. Whisper는 예외로
`disable_radix_cache = True`를 강제한다.

### 5.3 캐시 관리

`/flush_cache`는 radix 트리 전체, 요청 풀, KV allocator, grammar 캐시를 버리고 **메트릭까지 리셋한다.**
`ADMIN_OPTIONAL`, 즉 **기본 열림**(§1.4)이고 `GET`으로도 도달한다. 실행 중 작업에는 파괴적이지 않다:
스케줄러가 완전히 유휴가 아니면 `success=False`를 반환하는 no-op이고, `?timeout=`은 실패 대신 지연시킨다.
실행 중 요청을 선점할 수 있는 vLLM의 dev 모드 prefix 리셋보다는 덜 위험하지만, 여전히 모든 테넌트의
캐시를 한 번에 지우며 prefix별·salt별 flush는 없다.

`/hicache/storage-backend*` 계열은 L3 계층을 관리한다. attach(PUT), detach(DELETE), status(GET)는
각각 `--admin-api-key` 설정을 추가로 하드 요구한다. **clear 라우트 둘은 그렇지 않다** —
`POST /hicache/storage-backend/clear`와 deprecated `GET /clear_hicache_storage_backend`는
`ADMIN_OPTIONAL`만 달고 있으므로, 키 없는 서버에서는 포트에 닿을 수 있는 누구나 그 스토어를 가리키는
모든 노드가 쓰는 **공유** L3를 지울 수 있다.

특정 prefix를 조회·고정·프리페치하는 API는 없다. vLLM과 동일.

### 5.4 dorang의 청크 granularity가 문제가 되는 한 경우

§5.1은 *조회*에 대해 성립한다. dorang이 SGLang의 KV 이벤트 스트림을 캐시 인지 라우팅에 소비하기
시작하면 성립을 멈춘다. 그 스트림은 `page_size`와 같은 `block_size`를 publish하고 구독자가 정확히 그
크기로 해싱하기를 요구한다 — 소스 주석이 *"placeholder `block_size`는 라우터 쪽에서 잘못된 granularity로
프롬프트를 해싱해 조용한 KV 캐시 미스를 일으킨다"* 고 말한다. 이벤트는 페이지 크기 토큰 스팬에 대한
SHA-256 체인으로 만든 `parent_block_hash`를 실으며, 바이트 경계 청커는 그 키를 재현할 수 없다.

이것은 §7.4b를 바꿀 이유가 아니라 **KV 이벤트 라우팅을 범위 밖에 둘 이유**다: 그러려면 dorang이
핫패스에서 엔진의 정확한 페이지 크기로 토큰화해야 하고, 그것이 §7.4b가 이미 거부한 결함이며, 이제 두
번째 엔진의 증거가 붙었다.

---

## 6. OpenAI로부터의 프로토콜 divergence

얼마나 조용히 실패하는지 순.

| # | Divergence | 결과 |
|---|---|---|
| 6.1 | **`--tool-call-parser` 없이 `tools`를 보내면 툴 호출이 평문으로 실린 200을 반환한다.** 세 가드가 파서 부재로 단락되지만 툴은 여전히 프롬프트에 렌더링된다 | 모델이 네이티브 툴 문법을 `message.content`에 내보내고, `tool_calls`는 `null`, `finish_reason`은 `"stop"`. **모든 에이전틱 클라이언트가 에러 없이 깨진다.** vLLM은 이 상황에서 400을 낸다. §6에서 가장 심각도가 높다 |
| 6.2 | **한 서버에 에러 봉투 다섯 형태.** (a) OpenAI 라우트의 평평한 `{"object","message","type","param","code"}`, (b) 같은 에러의 스트리밍 시 *중첩* `{"error":{...}}`, (c) `/v1/responses`의 OpenAI-중첩, (d) `/v1/messages`의 Anthropic 형태, (e) auth 미들웨어의 `{"error": "Unauthorized"}` — 맨 **문자열** | OpenAI SDK가 (a)를 파싱하지 못한다. `code`는 정수 상태. `type`은 네 호출 경로에 따라 `"BadRequestError"`, `"BadRequest"`, `"Bad Request"`, 문자열화된 `"400"` 중 하나. **안정적인 판별자는 HTTP 상태와 `object == "error"` 뿐이다.** dorang은 COMPATIBILITY 7.1로 정규화해야 하고 `type`으로 분기해서는 안 된다 |
| 6.3 | **모든 스트리밍 청크가 `reasoning_content`와 `matched_stop`을 명시적 `null`로라도 싣는다.** 항상 직렬화되도록 *의도적으로* 기본값 없이 선언돼 있다 | COMPATIBILITY 2.1("없는 필드는 생략, null 금지")과 2.2("그 이상 없음")를 동시에 위반한다. dorang은 compat egress에서 둘 다 제거해야 한다. 비스트리밍 응답은 여기에 더해 요청하지 않은 `metadata: {"weight_version": ...}`까지 붙는다 |
| 6.4 | **`finish_reason`이 `abort`일 수 있다.** 내부 타입은 `stop`, `length`, `abort`이며 그대로 통과하고, `tool_calls`는 `stop`에서 사후 합성된다. `content_filter`/`function_call`은 `Literal`에 선언돼 있고 절대 생성되지 않는다 | vLLM이 내보내는 것과 같은 `abort` 값. **`repetition`은 여기 없다.** COMPATIBILITY §4에 `abort` 행이 필요하며, vLLM 때문에 이미 필요했다 |
| 6.5 | **알 수 없는 요청 필드는 OpenAI *와* Anthropic 모델 양쪽에서 조용히 버려진다.** 어느 모델도 `model_config`를 선언하지 않아 Pydantic v2가 `extra='ignore'`로 간다 | vLLM §2.4와 동일. 오타가 기본 동작과 함께 200을 반환한다. dorang은 두 엔진 중 어느 쪽의 검증에도 기댈 수 없다 |
| 6.6 | **두 OpenAI 엔드포인트의 `max_tokens` 기본값이 다르고, 한쪽은 `null`을 거부한다.** `/v1/completions`: `max_tokens: int = 16` — **non-Optional**이라 명시적 `null`은 **400**, vLLM은 16으로 강제한다. `/v1/chat/completions`: 둘 다 `None` 기본이고 `None`이 그대로 전달된다 | dorang은 두 엔진 모두에 `/v1/completions`로 실제 정수를 보내야 하고, SGLang에는 절대 `null`을 보내면 안 된다. `or` 연산에 주의: `max_completion_tokens: 0`이 조용히 `max_tokens`로 떨어진다 |
| 6.7 | **`usage.reasoning_tokens`가 `completion_tokens_details.reasoning_tokens`가 아니라 최상위 필드**이며 기본값 `0`이라 항상 방출된다. `total_tokens`는 그것을 제외한다 | 설계 §10.7의 `ReasoningTokens` 매핑에 SGLang 전용 읽기가 필요하다. 이 서버에 `completion_tokens_details`는 존재하지 않는다 |
| 6.8 | **`prompt_tokens_details`는 `--enable-cache-report` AND `cached_tokens > 0`이 아니면 `null`이다.** 플래그 기본값 `False` | VLLM.md §5의 `--enable-prompt-tokens-details` 행과 정확히 같다: **없으면 설계 §8의 비용 엔진이 캐시된 prompt 토큰을 정가로 과금한다** |
| 6.9 | **`tools`와 함께 온 `tool_choice: null`은 `"auto"`로 정규화된다** — 명시적 null이 조용히 툴 파싱을 끄는 vLLM과 *반대*다. 그러나 `tool_choice: "none"`은 **툴 스키마를 프롬프트에서 완전히 제거**하며, OpenAI는 여전히 모델에게 보여준다 | vLLM 때문에 만든 리터럴 `null` → `"auto"` 정규화는 여기서 무해하니 유지할 것. `"none"` 동작은 모델 출력을 바꾸는 실제 의미 차이다 |
| 6.10 | **`--reasoning-parser`가 필요하며, 없으면 `reasoning_content`가 항상 null이다.** 요청 기본값인 `separate_reasoning: true`는 플래그 없이 받아들여지고 무시되어 `<think>` 태그가 `content`에 인라인으로 남는다. 파싱 실패는 400이 아니라 **500** | vLLM의 `--reasoning-parser` 행과 같은 모양. **필드 이름이 vLLM과 다르다**: SGLang은 `reasoning_content`, vLLM 응답 필드는 `reasoning`. 설계 §10.2의 역매핑에 둘 다 필요하다 |
| 6.11 | **`model` 필드가 `/v1/chat/completions`나 `/v1/messages`에서 검증되지 않는다.** 검증 함수가 messages, tool_choice, 툴 스키마, 토큰 예산을 확인하고 모델 이름은 절대 확인하지 않는다 | vLLM은 모델 불일치에 404를 내고 VLLM.md §2.10은 그것을 *라우팅* 실패 시그니처로 옳게 읽는다. **SGLang은 아무 신호도 주지 않는다** — 잘못 라우팅된 요청이 로드된 아무 모델로나 서빙된다. dorang은 요청이 실패하기를 기대하는 대신 기동 시 `/v1/models`로 모델 이름을 검증해야 한다 |
| 6.12 | **모델 이름의 콜론이 LoRA 어댑터를 선택한다.** `model`을 첫 콜론에서 쪼개 접미사를 어댑터 이름으로 취급하고 `lora_path`를 override한다. `served_model_name`은 기동 시 콜론 없음이 assert된다 | `llama3:8b`나 `qwen:7b-instruct` 같은 Ollama 관례의 클라이언트 대면 alias가 조용히 재해석된다. dorang은 이 백엔드에 대해 업스트림 모델 이름에 콜론을 통과시키면 안 된다 |
| 6.13 | **`user`, `best_of`, `suffix`, `service_tier`, `store`, `metadata`, `CompletionRequest.custom_labels`가 전부 받아들여지고 버려진다** | vLLM은 미지원 필드 셋에 세 가지 다른 동작을 한다(§2.7). SGLang은 하나다: 전부 버린다. 더 단순하고 똑같이 탐지 불가능하다. **`service_tier`는 두 백엔드 어느 쪽에서도 priority 대체 수단이 되면 안 된다** |
| 6.14 | **`--allow-auto-truncate`는 컨텍스트 초과를 조용한 200으로 바꾼다.** 기본 `False`; 켜면 입력 길이 검사와 총 토큰 검사가 모두 예외 대신 경고 로그와 절삭으로 간다 | 그런 배포에서 dorang의 `context_window` 폴백은 **매칭할 시그니처가 없고**, 클라이언트는 절삭된 프롬프트에 대한 completion을 조용히 받는다. 이 플래그는 프로바이더 설정에 기록되고 capability 다운그레이드로 취급되어야 한다 |
| 6.15 | **`stream_options.include_usage: false`를 서버가 켜서 override할 수 있다.** `--stream-response-default-include-usage`가 모든 스트림에 usage를 강제한다 | COMPATIBILITY 3.1은 클라이언트가 요청했을 때만 usage를 요구한다. dorang은 요청되지 않은 usage 청크를 egress에서 제거해야 한다 |
| 6.16 | **추가 SSE 청크 타입.** `hidden_states` 청크와 `choices: []`인 `sglext` 청크가 스트림 도중 나타날 수 있다. usage 청크 둘의 직렬화도 다르다: chat은 null을 제외하지 않고 completions는 제외한다 | dorang의 SSE 누산기는 모든 프레임이 `choices[0].delta`를 갖는다고 가정하는 대신 알 수 없는 청크 형태를 견뎌야 한다 |

### 6.17 컨텍스트 윈도우 초과 시그니처

문구 다섯 가지, 어느 것도 vLLM의 것과 같지 않다:

| 조건 | 텍스트 |
|---|---|
| 입력만 | `The input (N tokens) is longer than the model's context length (M tokens).` |
| 입력 + max_tokens | `Requested token count exceeds the model's maximum context length of M tokens. ...` |
| 출력 예산 | `max_completion_tokens is too large: N.This model supports at most M completion tokens.` (빠진 공백은 소스 그대로) |
| 스케줄러 쪽 | `Input length (N tokens) exceeds the maximum allowed length (M tokens).` |
| 멀티모달 | `Multimodal prompt is too long after expanding multimodal tokens.` |

**VLLM.md §2가 권장한 부분 문자열 `"maximum context length"`는 이 중 두 번째만 매칭한다.** 엔진 간
공통 부분 문자열은 더 짧은 **`"context length"`** 로, vLLM의 세 문구와 SGLang의 앞의 둘을 매칭하고
SGLang의 나머지 셋은 여전히 놓친다.

정직한 결론: **§2의 사전 계산 경로를 우선할 것.** 시그니처 매칭은 backstop이고, 엔진별이어야 하며,
`--allow-auto-truncate` 배포에서는 아예 발화하지 않는다.

---

## 7. 정규화 표

canonical 이름은 설계 §10.7의 `CanonicalRequest` / `CanonicalResponse` 필드다.
표기: **=** 동일 · **~** 다르지만 매핑 가능 · **✗** 표현 불가.

### 7.1 요청

| Canonical | vLLM | SGLang | |
|---|---|---|---|
| `Model` | `model`; **불일치 시 404** | `model`; **검증 없음**, `:`가 LoRA 어댑터를 선택 | ~ |
| `Messages` | `messages[]` | `messages[]` | = |
| `System` | `messages[role=system\|developer]` | 동일; Anthropic `system`은 문자열로 평탄화 | = |
| `MaxOutputTokens` | `max_completion_tokens` → `max_tokens`; completions 기본 16, `null`은 16으로 강제 | 같은 우선순위; completions 기본 16이지만 **`null`은 400** | ~ |
| `TopK` | `top_k` (확장) | `top_k` | = |
| `Stop` | `stop` | `stop`, 추가로 `stop_token_ids`, `stop_regex` | = |
| `Tools` | `tools[].function`; **파서 플래그 없으면 400** | `tools[].function`; `--tool-call-parser` 없으면 **툴 문법이 평문으로 실린 200** (§6.1) | ~ |
| `ToolChoice` | `tool_choice`; 명시적 `null`이 **파싱을 끈다** | `null` → `"auto"` 정규화; `"none"`은 **프롬프트에서 스키마를 제거** | ~ |
| `ParallelToolCalls` | `parallel_tool_calls`; **응답을 후필터링** | grammar 생성으로 전달되나 `tool_choice:"auto"`이고 제약이 없으면 **강제 경로가 없다** | ~ |
| `ResponseFormat` | `response_format` + 중첩 structured-output 객체 | `response_format`, 추가로 `regex`, `ebnf`, `json_schema`, `structural_tag` | ~ |
| `Reasoning` | `reasoning_effort`; `--reasoning-parser` 필요 | `reasoning_effort`(확장: 0–0.99 float과 `"max"` 허용) + `separate_reasoning`; `--reasoning-parser` 필요 | ~ |
| `Seed` | `seed` | `seed`, 내부적으로 `sampling_seed`로 개명 | = |
| `Logprobs` | `logprobs`, `top_logprobs` | 동일, 그러나 OpenAI가 요구하는 `logprobs: true` 게이팅이 **없다** | ~ |
| `EndUser` | `user` — 선언되고 명시적으로 무시 | `user` — 선언되고 조용히 버려짐; Anthropic `metadata.user_id`도 버려짐 | ✗ |
| `Metadata` | `metadata` | chat/completions에 **요청 필드가 아니다** | ✗ |
| `ServiceTier` | `service_tier` — 받아들여지고 **소비자 0** | chat/completions에 **필드가 아니다** | ✗ |
| `CacheBreakpoints` | — | Anthropic `cache_control`이 `extra='ignore'`로 **버려짐** | ✗ |
| `Priority` | `priority`, **낮은 값 우선**, 기본 `fcfs`에서 무시 | `priority`, **기본 높은 값 우선**, `--enable-priority-scheduling` 없으면 무시; `/v1/responses`에서 죽어 있음; `/v1/messages`에 없음 | ~ **방향 반전** |
| `CacheSalt` | `cache_salt`; KV 캐시 분할 | `cache_salt` + `extra_key`, **구분자 없이 연결**; L1/L2는 분할하나 **L3 스토리지 계층은 아님**; `/v1/messages`로 설정 불가 | ~ |
| `RequestID` | `X-Request-Id` 무조건 존중 | `rid` 필드는 **죽은 코드**; 헤더 override는 `/generate` 전용 + 환경변수 게이트 | ✗ |
| `DPRank` | `X-data-parallel-rank` 헤더; 잘못된 값은 **조용히 무시** | `routed_dp_rank` 본문 필드 + `X-Data-Parallel-Rank` 헤더; 범위 밖은 **400** | ~ |

### 7.2 응답과 usage

| Canonical | vLLM | SGLang | |
|---|---|---|---|
| `StopReason` | `stop`, `length`, `tool_calls`, `content_filter`, **`abort`**, **`repetition`** | `stop`, `length`, `tool_calls`, **`abort`**. `content_filter`/`function_call`은 선언되고 미방출. `repetition` 없음 | ~ |
| `StopSequence` | — | **`matched_stop`** 이 모든 choice에 — 매칭된 토큰 id 또는 문자열. OpenAI 표면에 존재하고 **Anthropic adapter가 버린다** | ~ **SGLang 전용** |
| `InputTokens` | `usage.prompt_tokens` (캐시 read 포함) | `usage.prompt_tokens` (포함) | = |
| `CacheReadTokens` | `usage.prompt_tokens_details.cached_tokens`; `--enable-prompt-tokens-details` 필요 | 동일; `--enable-cache-report` 필요, **카운트가 0이면 `null`** | ~ |
| `CacheWriteTokens` | — | — (`cache_creation_input_tokens` 선언, 미기록) | ✗ |
| `ReasoningTokens` | — | **`usage.reasoning_tokens`, 최상위**, 항상 존재, 기본 0 | ~ **SGLang 전용, 비-OpenAI 위치** |
| `TotalTokens` | `usage.total_tokens` | prompt + completion, **reasoning 제외** | = |
| `ReasoningText` | 응답 `reasoning`, 요청에서 `reasoning_content` 허용 | 양방향 **`reasoning_content`** | ~ **필드 이름 다름** |
| 에러 봉투 | `{"error":{message,type,param,code:int}}` | **평평한** `{object:"error",message,type,param,code:int}`; 스트리밍일 때만 중첩 | ~ **구조적** |
| `logprob` sentinel | `-9999.0`이 `-inf` 대신 | 미관측; **미검증** | ? |
| 멀티모달 토큰 상세 | — | `prompt_tokens_details.{image,audio,video}_tokens` | ~ **SGLang 전용** |

### 7.3 Anthropic 표면

`/v1/messages`는 이벤트 목록과 블록 타입이 COMPATIBILITY 6.2와 일치하고 에러 봉투가 올바르다.
그러나 **✗ 행 다섯 개** — `StopSequence`(선언되고 미방출, 키는 null이 아니라 생략), `TotalTokens`(부재),
`CacheWriteTokens`(미기록), `CacheBreakpoints`(`cache_control`이 버려짐), `EndUser`(`metadata` 미독) —
그리고 서버 툴은 파싱된 뒤 INFO 로그와 함께 건너뛰며 `thinking.budget_tokens`는 경고만 하고 강제되지
않는다.

**이 다섯 ✗ 행이 dorang이 SGLang의 `/v1/messages`를 순수 passthrough로 취급할 수 없는 이유다.**
매우 좋은 adapter이지만 여전히 adapter이고, dorang 자신의 Anthropic egress 경로가 `stop_sequence`,
`total_tokens`, cache-write 카운터를 채워 넣어야 하며 `cache_control`은 사라지게 두는 대신 명시적으로
거부하거나 다운그레이드해야 한다.

---

## 8. 오퍼레이터 설정 프로필

표준 OpenAI·Anthropic 프로토콜 동작이 그대로 적용되려면 오퍼레이터가 무엇을 설정해야 하는가.
나란히, 그리고 없을 때 조용히 깨지는 것과 함께.

### 8.1 두 엔진 공통 런북

| 관심사 | vLLM | SGLang | 없으면 |
|---|---|---|---|
| 툴 호출 | `--enable-auto-tool-choice` + `--tool-call-parser <p>` | `--tool-call-parser <p>` | vLLM: **400**. SGLang: **툴 호출이 평문으로 실린 200.** SGLang 쪽이 위험한 실패다 |
| 리즈닝 | `--reasoning-parser <p>` | `--reasoning-parser <p>` | 리즈닝 필드가 항상 null이고 `<think>` 태그가 `content`에 남는다 |
| 캐시 토큰 과금 | `--enable-prompt-tokens-details` | `--enable-cache-report` | `cached_tokens`가 null → **설계 §8이 캐시된 prompt 토큰을 정가로 과금한다** |
| priority | `--scheduling-policy priority` | `--enable-priority-scheduling` | `priority`가 받아들여지고 무시되고 200 |
| 메트릭 | `--disable-log-stats`를 **주지 말 것** | `--enable-metrics` | vLLM: 시리즈 0개로 200, 유휴와 구별 불가. SGLang: **404**, 정직한 쪽 |
| 컨텍스트 상한 | `--max-model-len` | `--context-length` | 둘 다 `/v1/models`에 실효값을 보고한다. SGLang은 모델 자체 값보다 넓히기를 **거부**하고 vLLM은 경고한다 |

### 8.2 SGLang 전용 — 설정할 것

| 플래그 | 하는 일 | 없으면 |
|---|---|---|
| `--enable-priority-scheduling` | `priority`가 무언가를 하게 만든다 | 조용히 무시 (§3.4) |
| `--schedule-low-priority-values-first` | **방향에서 SGLang이 vLLM과 일치하게 만든다** | dorang의 `{realtime:0, batch:10}` 맵이 **뒤집힌다** — batch가 realtime을 앞지른다 |
| `--abort-on-priority-when-disabled` | 무시된 priority를 안정적 메시지의 503으로 바꾼다 | dorang이 priority 동작 여부를 프로브할 수 없다 |
| `--default-priority-value <n>` | 미지정 priority를 채운다 | 미지정 요청이 `±sys.maxsize`, 즉 맨 마지막에 스케줄되고 **게다가** `priority="None"` Prometheus 라벨을 만든다 |
| `--priority-scheduling-preemption-threshold 5` | 한 밴드가 다음 밴드를 선점할 수 있게 한다 | 기본 10에서는 dorang의 `{0,2,10}` 밴드가 아예 선점하지 못한다 (§3.5) |
| `--enable-cache-report` | `cached_tokens`를 채운다 | 비용 엔진이 모든 캐시 요청을 과다 청구 |
| `--tool-call-parser <p>` | 구조화된 `tool_calls` | §6.1 — 이 백엔드에서 최악의 조용한 실패 |
| `--reasoning-parser <p>` | `reasoning_content`를 채운다 | 항상 null |
| `--enable-metrics` | `/metrics`를 등록한다 | 404 |
| `--enable-metrics-for-all-schedulers` | rank별 스케줄러 게이지 | DP attention에서 모든 rank의 데이터가 TP-0 시리즈로 뭉개진다 |
| `--max-queued-requests <n>` | 대기 큐를 제한한다 | 무제한; 메모리가 자랄 때까지 큐가 자란다 |
| `--served-model-name <n>` | `/v1/models`에서의 모델 이름 | 전체 `model_path`가 기본. **콜론을 포함하면 안 된다** |

### 8.3 SGLang 전용 — 설정하면 **안 되는** 것

| 플래그 | 이유 |
|---|---|
| `--schedule-policy priority` | **구현 없는** 광고된 선택지; 스케줄러 초기화에서 크래시 (§3.3) |
| `--allow-auto-truncate` | 컨텍스트 초과를 **절삭된 프롬프트에 대한 조용한 200**으로 바꾼다 (§6.14). dorang의 `context_window` 폴백이 동작을 멈춘다 |
| `--tokenizer-worker-num > 1`을 `--api-key`와 함께 | multi-tokenizer 모드에서 **auth 미들웨어가 아예 설치되지 않는다.** 서버가 완전히 열린다 |
| `--stream-response-default-include-usage` | 클라이언트가 요청하지 않은 스트림에 usage를 강제, COMPATIBILITY 3.1 위반 |
| `--tokenizer-metrics-allowed-custom-labels`를 클라이언트 유래 값과 함께 | 라벨 *키*는 허용목록이지만 **값은 아니다** (§4.3) |

### 8.4 보안 자세 — SGLang 백엔드를 노출하기 전에 읽을 것

SGLang의 관리 표면은 vLLM보다 크고 기본적으로 열려 있다. 키를 설정하지 않은 기본 실행에서는 인증 없는
호출자가 radix 캐시를 flush하고, 생성을 멈추고, 스케줄러를 조이고, 가중치를 업데이트하고, LoRA 어댑터를
로드·언로드하고, `/server_info`를 읽을 수 있다.

dorang이 말을 거는 백엔드의 최소 자세:

1. **SGLang 포트를 신뢰 경계 밖으로 절대 노출하지 말 것.** `--api-key`와 `--admin-api-key` 모두
   레이트 리밋 없는 bearer 토큰 비교이고, `/health*`와 `/metrics`는 프리픽스로 그것들을 우회한다.
2. **`--api-key`에 더해 `--admin-api-key`를 설정할 것.** 그래야 `ADMIN_OPTIONAL` 라우트가 일반 키를
   받지 않게 된다.
3. **`/server_info`를 비밀을 담은 엔드포인트로 취급할 것.** `api_key`와 `admin_api_key`를 평문으로
   반환한다(§2.3). dorang이 capability discovery를 위해 읽는다면 프로바이더별 opt-in이어야 하고 응답은
   절대 로그되면 안 된다.
4. **`--api-key`가 설정된 서버에서는 `--tokenizer-worker-num`을 1로 유지할 것.**
5. **`--hicache-storage-backend`가 공유 스토어를 가리킨다면, 그 clear 라우트들은 admin 게이트가
   없고**(§5.3) `cache_salt`가 그 계층에서 테넌트를 격리하지 않음(§5.2)에 유의할 것. 공유 L3 스토어는
   인증 없는 wipe가 가능한 공유 테넌시 표면이다.
6. CORS가 `*` + credentials로 무조건 걸려 있다. 어떤 origin의 브라우저든 네트워크가 허용하는 모든
   라우트에 도달한다.

---

## 9. 미검증, 그리고 SGLang 문서가 소스와 어긋나는 곳

### 9.1 미검증

가정하지 않고 기록한다: 이 커밋의 릴리스 번호(shallow clone, 태그 없음); `POST /v1/tokenize`가 vLLM처럼
컨텍스트 길이 필드를 반환하는지; `-9999.0` 스타일 logprob sentinel을 내보내는지(그런 상수를 찾지 못했으나
sampling 서브트리가 체크아웃에 없었다); `/v1/chat/completions`에서 `max_new_tokens=None`이 하위에서
무엇으로 해결되는지(같은 누락 서브트리; 확인된 것은 그것이 `None`일 때 총 토큰 컨텍스트 검사가 완전히
건너뛰어진다는 점); `parallel_tool_calls`의 실제 강제 여부; `/v1/messages`가 tool-use 턴에서
`content_block_index`를 올바로 리셋하는지; `--enable-metrics-for-all-schedulers`가 `/v1/loads` 스냅샷을
바꾸는지; `enable_dp_attention` + `nnodes > 1`에서의 ZMQ load-snapshot 전송 동작;
`disable_finished_insert`가 어디서 true로 설정되는지; deterministic-inference와 `--enable-mis` 경로가
`sglang:cache_hit_rate` 게이지도 끄는지 아니면 §4.3의 리셋 버그와 구별되지 않는 0을 계속 내보내는지.

### 9.2 SGLang 자체 문서가 틀리거나 불완전한 곳

전 구간에서 소스를 우선했다.

| 주장 | 실제 |
|---|---|
| `--schedule-policy priority`가 허용값이다 | 구현 없음; `raise ValueError(f"Unknown schedule_policy: {policy=}")` |
| `sglang:num_retracted_reqs`는 *"retract된 요청의 수"* | 마지막 publish 이후의 델타이며 매번 0으로 리셋 |
| `sglang:token_usage` — 이름이 토큰 수를 암시 | 0–1 분수, 소수 2자리 반올림, `max(full, swa, mamba)`. 소스 자체에 `FIXME: misleadingly named` |
| `sglang:utilization` — *"the utilization"* | 영구 0, PD-prefill 모드에서는 `-1`; 입력에 setter 없음 |
| `sglang:is_cuda_graph` | 어디에서도 할당되지 않음; 영구 0 |
| `sglang:engine_startup_time`, `sglang:engine_load_weights_time` | 유일한 호출 지점에서 `0.0` 하드코딩 |
| Observability 문서: *"`--enable-metrics`를 추가해 켤 수 있다"* | 맞지만, 없으면 `/metrics`가 **404**라는 점과 `--enable-metrics-for-all-schedulers`의 존재를 빠뜨린다 |
| Anthropic 문서: *"SGLang은 요청 `model` 필드를 검증하지 않는다"* | `/v1/messages`와 `/v1/chat/completions`에는 맞지만, `GET /v1/models/{model}`은 불일치 시 **404를 낸다**. 한 서버의 두 엔드포인트가 모델 이름의 의미에 대해 서로 다른 말을 한다 |
| Anthropic 문서: *"Anthropic Messages API용으로 만든 어떤 클라이언트든 수정 없이 self-hosted SGLang 서버와 대화할 수 있다"* | 흔한 경로에 대해서는 참. `cache_control`, `metadata`, `stop_sequence`, `usage.cache_creation_input_tokens`, `thinking.budget_tokens`, 그리고 네 가지 서버 툴 계열에 대해서는 조용히 거짓 — 각각 받아들여지고 버려진다 (§1.2) |
| Anthropic 문서의 `--tool-call-parser` 주석: *"툴 스키마는 여전히 받아들여지지만 모델의 툴 호출이 raw 텍스트로 돌아온다"* | 정확하며 인정할 가치가 있다 — 문서가 §6.1의 실패를 명시하는데 소스는 그러지 않는다 |

### 9.3 실행할 가치가 있는 문서 발견 하나

SGLang 문서는 어떤 코딩 에이전트가 매 턴 해시가 바뀌는 요청별 attribution 블록을 시스템 프롬프트 앞에
붙여 radix prefix 재사용을 무력화한다는 점, 그리고 환경변수로 그것을 제거할 수 있다는 점을 기록한다.

**그 실패 모드는 SGLang 고유가 아니라 dorang을 포함한 어떤 게이트웨이에도 적용된다.** 프롬프트 머리
근처의 요청별 가변 토큰은 prefix 재사용을 그 앞의 것까지로 붕괴시킨다. dorang의 캐시 어피니티
설계(설계 §7.4)는 "클라이언트가 주입한 가변 prefix"를 일급 위험으로 다뤄야 하며, dorang 자신이 전달하는
프롬프트 앞에 요청 범위 메타데이터를 붙여 그런 것을 만들어내서도 안 된다.
