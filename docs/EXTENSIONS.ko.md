# 벤더 확장 API와 컨텍스트 윈도우 처리

> OpenAI 호환 명세 어디에도 없지만 게이트웨이가 반드시 다뤄야 하는 두 가지.
>
> 기준 문서의 모든 동작 주장은 소스에서 읽은 `file:line` 인용을 달고 있다. 소스로 확인할 수 없는
> 주장은 추측 대신 **UNVERIFIED**로 표시한다. 소스와 문서가 어긋나면 소스가 이기고 그 불일치를
> 명시한다. **인용 전문은 기준 문서에 있으며, 이 문서는 결론과 근거를 옮긴다.**
>
> 설계 §10.1(두 종류의 손실), §10.5a(dorang은 compaction하지 않는다), §10.6(범용 패스스루 엔진),
> §10.7(메타데이터 등가), 그리고 COMPATIBILITY와 함께 읽을 것.
>
> English (기준 문서): [EXTENSIONS.md](EXTENSIONS.md)

---

## 0. 이 문서를 읽는 법

### 0.1 인용 루트

경로는 줄이 읽을 만하게 유지되도록 루트에 상대적으로 적는다.

| 프리픽스 | 루트 |
|---|---|
| *(없음)* | 이 저장소, `` |
| `codex/` | `codex` — Codex CLI Rust 워크스페이스 |
| `grok/` | `grok` — grok CLI Rust 워크스페이스 |
| `hermes/` | `hermes-agent` |
| `jikji/` | `jikji` |
| `openclaw/` | `openclaw` |
| `opencode/` | `opencode` |
| `openharness/` | `openharness` |
| `claude-code/` | `claude-code` — 유출된 TypeScript 스냅숏, §G 참조 |
| `vllm/` | `vllm-project/vllm`, 커밋 `7aea73d`. VLLM.md가 도출된 것과 같은 트리 |

이 문서를 의뢰한 브리프가 주장한 것 중 셋이 성립하지 않는 것으로 드러났다. 존재하지 않는 vLLM
라우트(§0.2), 테스트로만 존재하는 jikji 소스 파일 넷(§D.4), 그리고 실제로는 ingress 전송 패키지인
`internal/press`를 compaction 기계로 서술한 것(§D.4). 각각은 조용히 우회하지 않고 그것이 나오는
자리에서 교정한다. 틀린 전제를 말없이 흡수하는 명세는 그것을 그대로 실어 나르기 때문이다.

### 0.2 브리프의 전제 하나가 틀렸고, 그 교정이 결론을 바꿨다

이 문서를 의뢰한 과제는 VLLM.md가 "`/v1/responses/compact` 라우트 계열을 언급한다"고 했다.
**그렇지 않고, vLLM에 그런 라우트는 존재하지 않는다.** `compact`라는 문자열은 VLLM.md 어디에도
없고, vLLM의 Responses 라우터는 정확히 세 라우트를 등록한다.

- `vllm/entrypoints/openai/responses/api_router.py:49` — `POST /v1/responses`
- `vllm/entrypoints/openai/responses/api_router.py:80` — `GET /v1/responses/{response_id}`
- `vllm/entrypoints/openai/responses/api_router.py:110` — `POST /v1/responses/{response_id}/cancel`

vLLM 체크아웃 전체에 대한 `compact` grep은 KV 커넥터, spec-decode, JSON 직렬화 용도만 돌려주고
라우트 모양은 하나도 없다.

`/responses/compact`는 실재하지만 **OpenAI/Codex 백엔드 라우트**이지 vLLM의 것이 아니다.

```rust
// codex/codex-rs/core/src/client.rs:159-165
const REALTIME_CALLS_ENDPOINT: &str = "/realtime/calls";
const RESPONSES_ENDPOINT: &str = "/responses";
const RESPONSES_COMPACT_ENDPOINT: &str = "/responses/compact";
// `/responses/compact`는 unary이므로 타임아웃이 스트림 이벤트 사이의 유휴 구간 하나가 아니라
// 응답 전체를 덮는다.
const COMPACT_REQUEST_TIMEOUT_IDLE_MULTIPLIER: u32 = 4;
const MEMORIES_SUMMARIZE_ENDPOINT: &str = "/memories/trace_summarize";
```

이 교정은 현학이 아니라 하중을 받는다. compaction이 vLLM 라우트였다면 dorang은 자신이 완전히 통제하는
self-hosted 백엔드에서 그것을 만나 추론할 수 있었을 것이다. 아니다. **dorang이 요청·응답 본문을 구성할
수조차 없는, 크리덴셜로 막힌 사유 백엔드의 라우트**이며, 그것은 곧장 passthrough 범주에 속하고 다른
어디에도 속하지 않는다.

---

## A. 벤더 확장 API 목록

설계를 이끄는 것은 분류 열이다. 세 값:

- **neutral** — `CanonicalRequest`/`CanonicalResponse`로 표현 가능하거나 §10.1에 따라 droppable.
- **opaque** — 바이트 단위로 그대로 중계해야 함. §B 참조.
- **gateway-owned** — dorang이 제거·재작성·거부해야 함. 그대로 중계하는 것 자체가 결함이기 때문.

### A.1 OpenAI, Codex를 통해

Codex는 조사한 클라이언트 중 확장이 가장 많고, **서버측 compaction 엔드포인트를 호출하는 유일한**
클라이언트다. base URL이 크리덴셜에 따라 달라진다 — 같은 클라이언트가 어느 크리덴셜을 로드했느냐에 따라
서로 다른 두 경로 공간을 말한다. 프로바이더별 라우트 맵이 하나의 전역 테이블일 수 없는 첫 번째 이유다.

**엔드포인트**: `POST {base}/responses/compact`(서버측 대화 compaction, 대체 히스토리를 반환),
`/memories/trace_summarize`, `/alpha/search`, WebSocket 위의 `/responses`, `/realtime/calls`와 `/live`,
`GET {base}/models?client_version=…`(ETag 포함), `/images/generations`·`/edits`,
`/api/codex/*` 또는 `/wham/*`(컨트롤 플레인, 설정으로 선택되는 두 경로 스타일),
`wss://…/wham/remote/control/server{,/enroll,/refresh,/pair,/pair/status}`.
경로 결합 규칙은 양쪽에서 슬래시 하나씩 잘라 이어붙이는 것이고 `/v1`을 넣지 않는다 — 설계 §10.6
2단계("프리픽스를 벗기고 프로바이더 base URL에 결합")와 같은 규칙이다.

**`/responses`의 비표준 요청 필드**:

| 필드 | 동작 | 분류 |
|---|---|---|
| `client_metadata` | `session_id`, `thread_id`, `turn_id`, `window_id`, `installation_id`, `compaction`, W3C traceparent를 담는 비표준 객체 | **opaque** |
| `include: ["reasoning.encrypted_content"]` | 항상 정확히 이 한 원소 | **opaque** |
| `store` | **Azure를 제외하면 `false`** | neutral |
| `prompt_cache_key` | 기본값이 **세션 id** | **opaque** |
| `reasoning.context: auto\|current_turn\|all_turns` | 비표준 하위 필드 | **opaque** |
| `stream_options.reasoning_summary_delivery` | 비표준 | **opaque** |
| `text.verbosity` | | droppable |
| `previous_response_id`, `generate` | **WebSocket 본문 전용**, HTTP 본문에는 없다 | **opaque** |
| `context_management: [{type: "compaction", compact_threshold: N}]` | **요청 필드로서의 서버측 compaction.** A.1a | **opaque** |
| `prompt_cache_retention: "24h"` | `api.openai.com`에만 전송 | **opaque** |

기억해 둘 부재 둘: `previous_response_id`는 HTTP 본문에 **없고**, `safety_identifier`는 워크스페이스
어디에도 없다.

#### A.1a `context_management` — 요청 필드로서의 서버측 compaction

`/responses/compact`가 이 백엔드의 유일한 서버측 compaction 메커니즘이 아니며, dorang에 도달할
가능성이 가장 높은 것도 아니다. 다른 클라이언트는 **평범한 `/responses` 요청의 필드**를 설정해
compaction을 요청한다:

```ts
if (policy.useServerCompaction && payloadObj.context_management === undefined) {
  payloadObj.context_management = [{ type: "compaction", compact_threshold: policy.compactThreshold }];
}
```

임계값은 **컨텍스트 윈도우의 70%**, 하한 1,000, 윈도우를 모를 때 기본 80,000이고, `store`가 명시적으로
`true`이고 프로바이더가 OpenAI일 때만 켜진다.

dorang 입장에서 §A에서 가장 중요한 행이다:

- **키로 삼을 라우트가 없다.** "compaction은 passthrough 트래픽"이라고 경로로 판단하는 게이트웨이는
  이것을 평범한 `/v1/responses` adapter로 보낸다.
- 요청 본문의 **중첩 객체 배열**이다. 알려진 필드 구조체로 디코드하고 다시 인코드하는 adapter는 조용히
  버리고, 호출자는 compaction도 에러도 없이 나중에 오버플로한다.
- **`store`에 결합된다.** dorang의 `Store` 필드(§10.7)와 `responses_store`(§9.2)에 세 번째 상호작용이
  생긴다: 호출자는 `store: true`가 변환을 살아남아야만 서버 compaction을 얻는다.

그래서 OpenAI만이, 조사한 벤더 중 유일하게, 서버측 compaction을 **서로 호환되지 않는 세 형태** —
전용 라우트, sentinel 입력 아이템, 요청 필드 — 로 제공한다. 어느 둘도 같은 메커니즘으로 탐지되지 않는다.

#### A.1b Codex 라우트 서버가 거부하는 필드

`chatgpt.com/backend-api/codex`와 말하는 클라이언트는 같은 벤더가 `api.openai.com`에서는 받아들이는
파라미터를 **제거해야** 한다: `max_output_tokens`, `metadata`, `prompt_cache_retention`,
`service_tier`, `temperature`, `top_p`. 그리고 `store: true`는 아예 거부된다 — *"Store must be set to
false"*. 재생되는 `input[].status`도 제거해야 한다.

이것은 능력을 `(kind, model)`에 키잉하기로 한 설계 §4.3의 결정과 `max_tokens` 필드 이름을 배포별 설정으로
만든 COMPATIBILITY 5.5의 결정을 직접 뒷받침한다: 여기서는 **같은 벤더, 같은 와이어 계열, 같은 모델**이
어느 크리덴셜로 연결했느냐에 따라 다른 파라미터 집합을 받는다. dorang의 별도 `codex-responses` kind는
옳은 형태이고, 빠진 것은 이 거부 파라미터 목록이다.

#### 헤더

요청: `x-codex-installation-id`, `x-codex-turn-state`, `x-codex-turn-metadata`,
`x-codex-parent-thread-id`, `x-codex-window-id`, `x-openai-memgen-request`, `x-openai-subagent`,
`x-responsesapi-include-timing-metrics`, `OpenAI-Beta`, `session-id`, `thread-id`,
`x-client-request-id`, `ChatGPT-Account-ID`, `X-OpenAI-Fedramp`.

`x-openai-subagent`는 요청이 compaction일 때 값 `"compact"`를 갖는다 — 본문을 파싱하지 않고 compaction
트래픽임을 게이트웨이에 알려주는 유일한 헤더다.

응답에서 Codex가 읽는 것: `x-reasoning-included`, `x-codex-turn-state`, `openai-model`,
`x-request-id`, `X-Models-Etag`, 그리고 큰 `x-codex-*` 레이트리밋 계열.

**요청이 zstd로 압축될 수 있다.** JSON 본문을 스니핑할 수 있다고 가정하는 passthrough는 여기서 틀린다.

### A.2 xAI, grok CLI를 통해

grok은 하나의 base URL 계열에 대해 **세 가지** 추론 와이어 포맷을 말한다: `/chat/completions`,
`/responses`, `/messages`(Anthropic Messages 형태).

| 필드 | 동작 | 분류 |
|---|---|---|
| `search_parameters` | `{mode, sources[], from_date, to_date, return_citations, max_search_results}` | **opaque** |
| `reasoning_effort` (`xhigh` 포함) | | neutral — §10.2가 이미 `xhigh`를 열거 |
| `x_search` 서버측 툴 | 타입 있는 툴 스키마로 표현할 수 없어 **직렬화 이후에 raw JSON으로 주입**된다 | **opaque** |
| `x-grok-conv-id`, `-req-id`, `-model-override`, `-session-id`, `-agent-id`, `-turn-idx`, `-deployment-id`, `-user-id` | 본문 필드가 아니라 헤더 | **opaque**, 단 `-model-override`는 예외 |
| `usage.cost_in_usd_ticks` | xAI 확장, 1 USD = 1e10 ticks | neutral, dorang이 읽어야 한다 |
| `usage.context_details.{input_tokens,output_tokens}` | A.2a — 발견된 usage 확장 중 가장 결과가 큰 것 | **opaque** |
| `message.citations[]`, `message.reasoning_content` | | `reasoning_content`는 neutral; `citations`는 구조적(OpenAI 등가 없음) |

`x-grok-model-override`는 opaque가 아니라 **gateway-owned**다. 본문도 `model`을 싣는데 헤더가 모델
id를 싣는다. 설계 §7.2는 본문의 model을 진짜 업스트림 id로 삼고 응답의 `model`을 alias로 되돌린다.
모델을 선택한다고 주장하는 메커니즘이 둘이라는 것이 바로 §7.2가 제거하려는 모호성이다. dorang은 해결된
배포에서 이 헤더를 스스로 설정하거나 보내지 않아야 하며, 클라이언트 값을 검토 없이 중계해서는 안 된다.

#### A.2a `context_details` — 클라이언트가 compaction할 때 바뀌는 usage 확장

grok은 `response.completed`/`response.incomplete`에서 API가 xAI 고유 `context_details`를 내보내면
`response.usage.total_tokens`를 **그 자리에서 실제 라이브 컨텍스트 길이로 다시 쓴다**. 소스의 이유:

> `total_tokens`는 CLI의 `/context` 바, 자동 compact 임계값, 그리고 영속화된 세션의 `meta.totalTokens`를
> 구동한다. 서버측 멀티턴 루프(`web_search`, `x_search` 등) 아래에서는 와이어의 누적 총합이 루프가 도는
> 동안 부풀고, `context_details`는 마지막 턴의 prompt + output 토큰 — 모델이 실제로 앉아 있는 라이브
> 컨텍스트 — 을 보고한다.

**usage 확장이 장식이 아니라는 것을 이 조사에서 가장 깨끗하게 보여주는 사례다.** 설계 §10.7은 usage를
`InputTokens`/`OutputTokens`/`TotalTokens`로 정규화하며 "토큰 회계는 정확히 맞아야 하는 부분"이라고
단언한다. 그것은 여기서 **호출자가 언제 compaction할지를 결정하는 값**이기도 하다. 타입 있는 필드에서
`total_tokens`를 재계산하는 게이트웨이는 클라이언트가 의도적으로 대체한 누적 값을 되돌려주고, 클라이언트의
compaction이 엉뚱한 시점에 — 검색 루프에서는 너무 이르게, 어디에도 에러 없이 — 발화한다. §10.7의
정규화는 백엔드가 준 `context_details`를 덮어쓰는 게 아니라 **보존해야** 한다.

### A.3 vLLM

vLLM에는 compaction 엔드포인트가 **없지만**(§0.2), 조사 전체에서 유일하게 발견된 *서버측 컨텍스트 관리*
메커니즘이 있고, 비표준 라우트 표면이 넓다. 특히:

- `POST /tokenize`, `/detokenize`, `GET /tokenizer_info`(플래그 게이트) — **opaque** 또는 gateway-owned.
- `POST /v1/load_lora_adapter`, `/v1/unload_lora_adapter` — **opaque**, 오퍼레이터 게이트.
- `POST /reset_prefix_cache`, `/reset_mm_cache`, `/reset_encoder_cache` — **노출하면 안 된다.**
  VLLM.md §5가 이미 개발 모드 엔드포인트를 프로덕션 백엔드에 호출하면 안 된다고 말한다; 이것이 그
  캐시 관리 계열이고, 답은 파괴적이라는 것이다.
- `/sleep`, `/wake_up`, `/is_sleeping`, `/pause`, `/resume`, `/abort_requests`, weight-update 계열,
  `/collective_rpc`, `/server_info` — **노출하면 안 된다.**
- `POST /v1/responses/{id}/cancel` — **opaque**. dorang의 §2.1 라우트 표에 **없다**.
- `GET /v1/responses/{id}?starting_after=&stream=` — 두 쿼리 파라미터가 확장이다.

또한 dorang의 §2.1 표는 `DELETE /v1/responses/{id}`를 열거하는데 **vLLM은 구현하지 않는다.** 라우트
표와 백엔드가 양방향으로 어긋난다.

#### A.3a `truncation`과 `truncate_prompt_tokens` — 서버측 컨텍스트 관리

실제로 존재하는 OpenAI 호환 서버 위의 서버측 compaction에 가장 가까운 것이며, compaction이 아니라
절삭이다.

```python
truncation: Literal["auto", "disabled"] | None = "disabled"
→ truncate_prompt_tokens = -1 if truncation != "disabled" else None
```

`-1`은 "전체 윈도우"를 뜻한다. 효과는 `truncate_prompt_tokens`가 설정되면 입력 길이가 예외를 던지는
대신 **클램프**된다는 것이고, 따라서 `ValueError("Input length … exceeds model's maximum context
length")`가 절대 발화하지 않는다. chat completions와 completions는 `truncate_prompt_tokens`와
`truncation_side: "left" | "right"`를 vLLM 확장으로 직접 노출한다.

dorang에 대한 결과 셋:

1. **`truncation: "auto"`는 자기 대화를 조용히 짧게 만들어 달라는 호출자의 명시적 요청이다.** 그것은
   호출자의 것이다. dorang의 것이 아니며 dorang이 설정해서는 절대 안 된다.
2. **§7.6의 `context_window` 폴백이 키로 삼는 바로 그 400을 억제한다.** 더 큰 모델로 라우팅됐을 요청이
   절삭된 프롬프트로 응답되며 `200`을 받는다. dorang이 `params.set{}`이나 kind 기본값으로
   `truncation: "auto"`를 주입한다면, 배포 전체에서 자기 폴백 경로를 꺼 버리는 셈이다.
3. 설계 §10.5a의 경고 — 호출자가 묻지 않은 질문에 대한 그럴듯한 답, 그리고 응답에는 그것을 말할 것이
   아무것도 없음 — 의 구체적 사례다.

#### A.3b vLLM Responses 계열 필드

`background`(`store: true` 요구), `store`(기본 **`true`**), `previous_response_id`,
`prompt_cache_key`(*받아들여지고 무시*되며 필드 설명이 스스로 그렇게 말한다), `cache_salt`,
`enable_response_messages`, `previous_input_messages`(Harmony 포맷, `previous_response_id`와 상호 배타),
`request_id`, `priority`(설명이 VLLM.md §1.2가 기록한 거짓말), `include_reasoning`.

`prompt_cache_key`는 VLLM.md §8의 "vLLM 문서가 틀린 곳" 표에 새로 추가될 항목이다 — 다만 여기서는 필드
자신의 설명이 정직하고 지켜지지 않는 쪽이 *OpenAI* 계약이다. 설정한 호출자는 에러도 캐싱도 얻지 못한다.

### A.4 GLM/Z.AI, MiniMax, Anthropic

#### GLM / Z.AI

base URL이 넷이며 그중 둘은 별도의 *coding* 플랜 경로다.

| 필드/라우트 | 동작 | 분류 |
|---|---|---|
| `thinking: {type, clear_thinking}` | GLM 리즈닝 형태 | neutral — 설계 §4.3의 `glm_thinking` |
| `reasoning_effort: "max"` | OpenAI가 정의하지 않는 레벨 | neutral — §10.2가 선언된 레벨 집합으로 클램프하므로 `max`가 카탈로그 레벨 목록에 *있어야* 하고 아니면 드롭된다 |
| `tool_stream: true` | 툴 호출 델타 스트리밍을 위한 최상위 플래그 | **opaque** |
| `GET …/monitor/usage/quota/limit` | 비표준 쿼터 엔드포인트 | **opaque** — 다만 dorang이 소비할 수도 있다 |
| `finish_reason: model_context_window_exceeded` | 비표준 종료값, 에러 텍스트로 노출됨 | COMPATIBILITY §4에 행이 필요 |

`do_sample`은 openclaw 어디에서도 쓰이지 않는다. 공개 API 문서에는 있고 이 조사의 어떤 클라이언트도
보내지 않는다. 부재가 누락으로 오해되지 않도록 기록한다.

#### MiniMax

텍스트 chat이 **Anthropic 형태**이며, 이는 설계 §4.3의 `minimax: { api: anthropic-messages }` 매핑을
확인해 준다.

| 필드/라우트 | 동작 | 분류 |
|---|---|---|
| `base_resp: {status_code, status_msg}` | **HTTP 200과 함께 반환되는 에러 봉투** | **gateway-owned** |
| `POST /v1/coding_plan/search` | 벤더 검색, 본문이 `{query}`가 아니라 `{q}` | **opaque** |
| `GET /v1/token_plan/remains` | 쿼터 | **opaque** |
| `/v1/t2a_v2`, `/v1/image_generation`, `/v1/video_generation` 등 | 미디어 | **opaque** |
| 혼합 프로토콜 SSE | 레거시 Anthropic 호환 스트리밍 엔드포인트가 네이티브 Anthropic thinking 블록이 아니라 **OpenAI 스타일 델타 청크로 `reasoning_content`를 반환** | 구조적 위험 |

`base_resp` 봉투는 opaque가 아니라 **gateway-owned**로 분류할 가치가 있다. 그대로 중계하는 것 자체가
결함이기 때문이다. 호출자는 "쿼터 초과"를 뜻하는 본문과 함께 `200 OK`를 받는다. dorang에게 이것은
클라이언트 문제만이 아니라 **계측과 폴백** 문제다: §7.6은 HTTP 상태로 실패를 분류하고, `200`으로 도착한
쿼터 소진은 재시도되지도 장애로 기록되지도 않는 반면 §8은 그것을 성공 요청으로 과금한다.

또한 MiniMax의 usage 엔드포인트는 이름을 잘못 붙였다 — 값은 소비량이 아니라 **잔량**이다. 설계 §6.2는
프로바이더 보고 쿼터를 로컬 계측과 결합하는데, *잔량*을 *사용량*처럼 결합하면 부호가 뒤집힌다.

혼합 프로토콜 SSE 행은 COMPATIBILITY 6.6이 "게이트웨이 전체에서 가장 복잡한 상태 기계"라 부르는 것에
대한 직접적 위협이다: Anthropic 프레이밍인데 OpenAI 형태 델타를 싣는 스트림은 어느 adapter의 가정도
만족시키지 않는다.

#### Anthropic

| 필드 | 동작 | 분류 |
|---|---|---|
| `cache_control: {type: "ephemeral", ttl: "1h"}` | 캐시 breakpoint의 TTL | **opaque** — `CapCacheBreakpoints`는 breakpoint를 모델링하지 TTL을 모델링하지 않는다 |
| `redacted_thinking: {data}` | 보류된 리즈닝 블록을 위해 재생되는 opaque 페이로드 | **opaque**; `canonical.Thinking.Redacted`는 있으나 *데이터*가 살 곳이 없다 |
| `usage` → 파생 **`contextUsage`** | 와이어 필드가 아니라 클라이언트가 Anthropic usage에서 계산해 저장하는 반복별 컨텍스트 값 | §B.2 B12 |
| `context_management: {edits: [...]}` + `anthropic-beta: context-management-2025-06-27` | **`/v1/messages` 위의 서버측 컨텍스트 편집.** A.4a | **opaque** |
| `cache_edits` | 자체 beta 헤더 아래의 캐시 편집용 동반 본문 필드 | **opaque** |

#### A.4a `/v1/messages` 위의 `context_management` — 위험도 최고 사례

OpenAI의 것(§A.1a)과 **같은 필드 이름에 전혀 다른 스키마**이며, 다른 벤더의, COMPATIBILITY §6이
"위험도 최고"라 부르는 표면 위에 있다. 전략은 둘: `clear_tool_uses_20250919`,
`clear_thinking_20251015`. 본문에는 요청의 beta 목록에 그 beta 헤더가 있을 때만 실린다. 그리고 응답에서
**왕복한다**.

dorang에게 §A에서 가장 어려운 행인 이유 넷:

1. **필드/헤더 쌍이다.** 본문 필드를 전달하면서 `anthropic-beta: context-management-2025-06-27`를
   버리면 서버가 거부하거나 무시하는 요청이 된다. 헤더만 전달하는 것은 무해하지만 무의미하다. 둘은 한
   단위다.
2. **`/v1/messages` 위에 있고, COMPATIBILITY §6은 그것을 "passthrough가 아니라 adapter"라고 말한다** —
   Anthropic 형태 요청이 OpenAI 형태 백엔드를 겨냥하는 일이 아주 흔하기 때문이다. 따라서 이 필드는
   *디코드될 것이고*, 접어 넣을 OpenAI 계열 등가가 없다. 변환된 요청에서의 부재는 construct id 없는
   구조적 손실이다.
3. **응답에도 나타나므로**, 스트리밍 adapter의 terminal 메시지 조립(COMPATIBILITY 6.6)이 다른 어떤
   계열에도 없는 필드를 실어야 한다.
4. **이름이 OpenAI의 것과 충돌한다.** Responses에서는 `{type, ...}`의 배열이고 Messages에서는
   `{edits: [...]}` 객체다. 이름으로 한 번 모델링하는 중립 표현은 둘 중 하나에 대해 틀린다. 반드시
   계열별로 키잉된 `Extra`에 남아야 한다.

기본값도 실제 운용 지점을 기록한다: `DEFAULT_MAX_INPUT_TOKENS = 180_000`,
`DEFAULT_TARGET_INPUT_TOKENS = 40_000`. 툴 클리어링은 내부 사용자로 게이팅되므로 공개 배포는 못 볼 수도
있다 — 모델링할 이유가 아니라 opaque로 취급할 이유다.

`contextUsage`는 §A.2a 발견의 두 번째 독립 확증이다. 한 클라이언트의 compaction 트리거는 그것을
`totalTokens`보다 선호한다. **두 벤더, 두 클라이언트, 같은 결론: `total_tokens`는 언제 compaction할지를
결정하는 숫자가 아니며, 그 숫자는 벤더별이다.**

**`/v1/messages/count_tokens`는 openclaw 어디에서도 쓰이지 않는다.** 모든 사전 회계가 문자 휴리스틱 +
직전 턴의 `usage`다. dorang은 그 라우트를 T0로 서빙하며 SDK 호환에 옳지만, 조사한 어떤 클라이언트도
호출하지 않는다는 것은 §D.2의 추정기가 얼마나 많은 짐을 져야 하는지에 대한 증거다.

### A.5 벤더 무관 패턴

벤더와 무관하게 여섯 형태가 반복되며, 패스스루 엔진은 그것들을 위해 만들어져야 한다:

1. **클라이언트가 에코하는 서버측 대화 핸들** — `previous_response_id`, `store`.
2. **클라이언트가 에코하는 암호화된 blob** — `reasoning.encrypted_content`,
   `Compaction { encrypted_content }`, Anthropic `thinking.signature`, `redacted_thinking.data`.
3. **헤더 안의 sticky 라우팅 토큰** — `x-codex-turn-state`, `x-grok-*` 계열, `chatgpt-account-id`.
4. **캐시 분할 키 또는 정책** — `prompt_cache_key`, `prompt_cache_retention`, `cache_salt`,
   `cache_control.ttl`.
5. **클라이언트 자신의 제어 루프가 읽는 usage 확장** — `context_details`, `contextUsage`,
   `cost_in_usd_ticks`, `cache_creation_input_tokens`, `prompt_cache_hit_tokens`, `usage.cost`.
6. **성공 상태로 실패를 보고하는 봉투** — MiniMax `base_resp`; z.ai가 오버플로를 조용히 받아들이고
   `usage`로만 보고하는 것.

여섯 중 어느 것도 OpenAI 호환 명세에 없다. 여섯 모두 조용히 깨지고, 여섯 번째는 호출자의 것이 아니라
**dorang 자신의 실패 분류**를 깨뜨린다.

---

## B. 무엇이 불투명하게 통과해야 하는가, 그리고 왜

### B.1 droppable, structural과 나란한 세 번째 범주

`internal/canonical`은 능력을 `Structural`과 `Droppable`로 나눈다. droppable 능력의 부재는 노브가
적용되지 않았고 dorang이 그렇게 말한다는 뜻이고, structural 능력의 부재는 dorang이 정보를 버린 채
`200`을 반환하게 된다는 뜻이다.

**opaque state는 세 번째 범주이고, 나뉘는 축이 다르다.** 구조적 손실은 프로토콜이 *무엇을 표현할 수
있는가*에 관한 것이다. opaque state는 dorang이 *무엇을 만질 자격이 있는가*에 관한 것이다. blob은
완벽하게 표현 가능하면서 — 그것은 JSON 문자열이다 — 여전히 dorang이 정규화·재포맷·재키잉·합성해서는 안
되는 것일 수 있다. 그 의미가 그것을 발행한 서버에 살기 때문이다.

한 번도 본 적 없는 필드에 적용할 수 있도록 진술한 판정 기준:

> 어떤 필드가 **opaque state**인 것은, 그 값의 정확성이 dorang이 아닌 어딘가에서 확립되고 dorang이 그것을
> 재도출할 수 없을 때다. 무결성 보호된 컨텐트, 서버측 핸들, 캐시 분할 키가 모두 해당한다. 결과적으로
> 유일하게 옳은 처리는 바이트 단위 중계이고, 유일하게 옳은 실패는 거부다.

설계 §10.2는 이미 한 사례에 정확히 이 규칙을 적용한다 — "무결성 자료를 담은 리즈닝 블록은 response id로
키잉된 opaque 핸들로 저장되고 바이트 단위로 재생된다 … dorang은 절대 그것을 조작하거나 재서명하지
않는다." §B는 그것을 일반화한다.

### B.2 목록

| # | 항목 | 왜 opaque인가 | dorang이 정규화하면 깨지는 것 |
|---|---|---|---|
| B1 | `reasoning.encrypted_content` | 서버 암호화; 클라이언트측 검증이 어디에도 없다 | 드롭: 툴 루프 2턴에서 리즈닝 연속성이 유실되고, 실패가 두 번째 턴에 착지한다 — 설계가 이미 최악의 발견 지점이라 부르는 곳. **다른 모델 계열로 전달: 하드 400.** grok에 그 에러 텍스트가 있고 *절대 재시도 불가*로 분류돼 있다 |
| B2 | `Compaction { id, encrypted_content }` (`encrypted_content`는 optional이 아니다) | 그것이 곧 compaction된 대화록이다. compaction 경계 이전의 모든 것은 그 안에만 존재한다 | 드롭 또는 재작성: 대화가 경계 이전 히스토리 전체를 잃고 모델이 파편에서 답한다. dorang은 그것을 재생성할 수 없다 |
| B3 | `CompactionTrigger {}` | 소스 주석이 명시적이다: *"compaction trigger는 요청 제어이지 영속 응답 아이템이 아니다."* | `input[]`을 알려진 아이템 타입 enum으로 검증하는 게이트웨이는 거부하고, 아이템을 `responses_store`에 히스토리로 영속화하는 게이트웨이는 제어를 히스토리로 저장한다. 둘 다 remote compaction v2를 깨뜨리며, 그것은 **평범한 `/responses` 위에 in-band로** 탄다 — 키로 삼을 라우트가 없다 |
| B4 | `x-codex-turn-state` | 백엔드 내부 어피니티 토큰. 턴당 한 번 설정되고 절대 수정되지 않는다 | 제거: 턴 도중 백엔드 어피니티가 유실된다. 발행한 것과 *다른* 배포로 전달: 잘해야 무의미, 나쁘면 낡은 곳으로 라우팅. COMPATIBILITY 7.3이 여섯 인증 헤더를 전달 전에 제거하라고 요구하는데 이것은 살아남아야 하므로, "인바운드 클라이언트 헤더 제거"가 일괄 규칙일 수 없다 |
| B5 | `previous_response_id` | dorang이 갖고 있지 않은 대화 상태로의 서버측 핸들 | 이미 처리됨: `responses_store`가 바로 이것을 위해 존재한다. 새 발견은 **필드 안정성 요구**다: Codex는 주변 요청 필드가 일치할 때만 입력 델타만 보낸다. dorang이 §10.3의 `params.set{}`/`params.default{}`로 파라미터를 주입하면 턴 사이에 서버가 보는 요청이 달라지고, 옛 형태에서 발행된 response id에 대해 그렇게 된다 |
| B6 | `previous_input_messages` (Harmony) | 벤더 사유 메시지 인코딩 | 교차 프로토콜 변환이 망가뜨린다. `previous_response_id`와의 상호 배타 제약도 dorang이 다른 쪽을 추가해 위반해서는 안 된다 |
| B7 | `prompt_cache_key` | 호출자가 고른 캐시 분할 정체성 | 재작성 또는 드롭: 캐시 적중률이 **에러도 신호도 없이** 붕괴하고, 설계 §8의 비용 엔진이 캐시 read였어야 할 토큰을 정가로 과금한다. 더 나쁘게, dorang이 *생성*하면 — 예컨대 자기 세션 키에서 — 호출자가 관리하던 캐시를 조용히 재분할한다 |
| B8 | `cache_salt` | 성능 파라미터가 아니라 **보안** 파라미터 | 드롭: 호출자가 갖고 있다고 믿는 프롬프트 격리를 잃는다. dorang이 주입: 공유 시스템 프롬프트의 테넌트 간 재사용이 파괴되고 이는 직접적 처리량 비용. VLLM.md §4가 이미 프로바이더별 명시 옵션·기본 꺼짐이어야 한다고 말하며, §B는 그것이 **제거되어서도** 안 되는 이유다 |
| B9 | `client_metadata` | `compaction`, `turn_id`, 스레드 계보, W3C `traceparent`/`tracestate`를 싣는다 | 알려진 필드 구조체로 디코드하고 다시 인코드하는 게이트웨이가 버린다. 트레이스 컨텍스트가 유실되고, 이 턴이 compaction이었는지에 대한 클라이언트 자신의 기록도 유실된다 |
| B10 | 응답 아이템 `id`와 프리픽스 규칙 | 프리픽스가 서버 발행 id와 클라이언트 로컬 id를 구별한다 | dorang이 id를 재작성하면 — 예컨대 COMPATIBILITY 5.4에 따라 tool-call id를 정규화하다가 — 클라이언트 로컬 id를 서버 발행처럼 보이게 하거나 서버의 것을 파괴할 수 있다. 툴 이름 단축(5.3)에는 이미 왕복 매핑이 있다; id에도 같은 규율이 필요하거나 아예 손대지 않아야 한다 |
| B11 | 직렬화 *이후* raw JSON으로 주입되는 `x_search`와 동류 | 타입 있는 툴 스키마 밖에 의도적으로 있다 | `tools[]`를 `canonical.Tool`로 디코드하고 다시 인코드하는 게이트웨이가 버린다. grok 자신도 거울 문제를 방어해야 한다 — API가 `tools`를 되돌려주고 자기 역직렬화기가 실패하므로, 알 수 없는 툴을 제거하고 재시도한다 |
| B12 | `usage.context_details` | 누적 `total_tokens`와 의도적으로 어긋나는 라이브 컨텍스트 값 | §A.2a. 덮어쓰면 **호출자의 compaction이 엉뚱한 시점에 발화하고**, 응답 어디에도 그것을 말하는 것이 없다. usage 필드 정규화가 청구서의 숫자가 아니라 제어 흐름을 바꾸는 유일한 항목이다 |
| B13 | `usage.cost_in_usd_ticks` | 요청에 대한 벤더 권위 가격 | 드롭: 백엔드가 이미 사실을 말했는데 설계 §8이 규칙 기반 가격으로 되돌아간다. tick 단위(1 USD = 1e10)와, `0`이 무료가 아니라 미청구를 뜻한다는 점에 유의 |
| B14 | `comp_hash` | *"compaction 호환 모델 설정에 대한 불투명 식별자"*; 바뀌면 compaction이 강제된다 | dorang의 alias 레이어(§7.2)는 하나의 클라이언트 대면 이름을 여러 실제 배포로 매핑할 수 있다. `GET /v1/models`가 서로 다른 `comp_hash`를 가진 두 모델을 앞세운 alias에 대해 하나를 보고하면, 클라이언트는 불필요하게 compaction하거나 해야 할 때 하지 못한다 |
| B15 | 모델 `ETag` / `X-Models-Etag` | 모델 카탈로그용 캐시 validator | dorang의 `GET /v1/models`는 호출 키의 허용목록으로 목록을 재작성하므로(COMPATIBILITY 7.4), 업스트림 ETag를 전달하면 dorang이 보내지 않은 본문을 validate하게 된다. 스트림에 실려 오는 것은 다른 문제다: §7.2 스캐너가 통과시키는 SSE 프레임 안에 도착하며 그것은 옳지만, 클라이언트가 dorang이 통제하는 카탈로그를 무효화하도록 초대한다 |
| B16 | Anthropic `thinking.signature` | 무결성 자료 | 이미 올바르게 모델링돼 있다. 목록의 완결성을 위해, 그리고 grok의 Messages 경로가 그것을 생성하기 때문에 기록한다 |
| B17 | `context_management: [{type: "compaction", compact_threshold}]` | 서버에게 compaction하라고 지시하는 중첩 요청 본문 구조 | 디코드/재인코드 adapter가 버린다: 호출자는 compaction도 에러도 없이 나중 턴에 오버플로하고, 실패가 원인에서 멀리 나타난다. `store: true`에도 결합되므로 `store`를 버리면 부수효과로 이것도 꺼진다 |
| B18 | `redacted_thinking.data` | 프로바이더가 텍스트를 보류한 리즈닝 블록의 opaque 페이로드 | `canonical.Thinking`은 `Redacted bool`을 모델링하지만 데이터 필드가 없다. `CanonicalRequest` 왕복이 블록을 **페이로드 없이** 재구성하며, 이는 §10.2가 signature에 대해 이미 고친 것과 같은 부류의 결함이다 |
| B19 | `usage.contextUsage`(Anthropic, 파생)와 `usage.context_details`(xAI, 네이티브) | 누적 총합과 구별되는 라이브 컨텍스트 값 | B12 참조. 두 독립 클라이언트, 두 벤더, 같은 동작 |
| B20 | `prompt_cache_retention: "24h"` | 키가 아니라 캐시 **정책** | 드롭: 캐시 엔트리가 기본 스케줄로 만료되고 호출자의 비용 모델이 틀린다. 주입: 과금될 수 있는 표면에서 호출자가 요청하지 않은 보존을 dorang이 연장한다 |
| B21 | `cache_control.ttl: "1h"` | Anthropic breakpoint 위의 같은 것 | `CapCacheBreakpoints`는 breakpoint의 *존재*를 모델링하지 TTL을 모델링하지 않는다. breakpoint를 그것이 없는 프로토콜로 넘기는 것은 이미 구조적 다운그레이드다; breakpoint는 있고 TTL 개념이 없는 곳으로 넘기는 것은 캐시 경제를 조용히 바꾼다 |
| B22 | Anthropic `context_management: {edits: [...]}` **+** beta 헤더, 응답에도 에코 | 전략 이름으로 버저닝된 서버측 컨텍스트 편집 프로그램 | 필드 드롭: 서버가 편집하지 않고, 호출자의 대화가 그가 넘지 않도록 준비한 한계를 넘어 자란다. **헤더는 드롭하고 필드는 유지: 요청이 거부되거나 필드가 무시된다** — 둘은 한 단위(§A.4a). 응답 필드 드롭: 편집이 일어난 바로 그 턴에 호출자가 그 사실을 알 수 없다. 그리고 이것이 `/v1/messages` 위에 있으므로 COMPATIBILITY §6의 adapter 경로가 디코드한다 — 따라서 조용한 생략이 아니라 §10.1에 따른 construct id와 `400`이 필요하다 |
| B23 | `cache_edits` | 캐시 편집 프로그램 | B22와 같은 필드/헤더 결합 |

### B.3 아무도 말하지 않은 라우팅 결과

B1과 B14가 함께 만드는, 오늘의 설계에 없는 제약:

**어떤 대화가 특정 배포가 발행한 opaque state를 담는 순간, 그 대화는 그것을 받아들일 수 있는 배포에
고정(pinned)된다.** §7.4a의 캐시 어피니티식 "선호"가 아니라 *고정*이다. 근거는 grok이 교차 계열 사례를
절대 재시도 불가로 분류한다는 것이고, 이는 §7.6의 fail-back이 도움이 되지 않음을 뜻한다: 형제 배포에서
재시도하면 동일한 400이 나온다.

설계 §10.2는 자신이 고려한 한 사례에 대해 이미 옳은 답에 도달했다 — "호출자가 dorang이 선택된 백엔드에
대해 만들 수 없는 블록을 에코하면, capability 라우팅이 만들 수 있는 백엔드를 선호한다. 없으면 dorang은
`400`으로 실패한다." §B는 **B.2의 모든 모델 계열 범위 항목**에 같은 규칙이 적용되어야 하고, §7.6에
대한 결과가 단순한 라우팅 선호가 아니라 **새로운 비-폴백 원인**이라고 말한다.

---

## C. 일급 관심사로서의 compaction

설계 §10.5a가 게이트웨이 문제를 결론짓는다: **dorang은 compaction하지 않는다.** 이 절은 그 근거를
제공하며, 그것은 예상할 법한 논증이 아니다. compaction하는 클라이언트들은 그 일에 허술하지 않다.
극도로 신중하다 — 그리고 그 신중함이야말로 게이트웨이가 복제할 수 없는 것이다. 전부 에이전트 루프만이
아는 것에 의존하기 때문이다.

### C.1 클라이언트는 언제 compaction하는가

| 클라이언트 | 트리거 |
|---|---|
| Codex | **해결된 컨텍스트 윈도우의 90%**, 서버가 한계를 보내지 않으면 도출하고 보내면 클램프로 사용 |
| Codex | **95% 실효 윈도우**를 먼저 적용, 시스템 프롬프트·툴 오버헤드·출력을 위한 여유 확보 |
| grok | **85%**, 두 하네스 공유 |
| grok | **투기적 패스는 10포인트 이르게**(75%) |
| hermes | **기본 50%**, 512K 미만 윈도우에서는 **75%** 로 상향, 상향 전용 |
| hermes | 특정 모델군에 85% / 70% / 75% |
| jikji | **90%** 기본 소프트 트리거; 캐시 인지 지연을 위한 밴드 `(0.8, 0.95]` |
| jikji | 관측 예산 수축과 모델 대면 사용 지시에 **80%** |
| openclaw | **퍼센트가 아니라 절대 예비**: `contextTokens > contextWindow − 16384`일 때 compaction |
| openclaw | 에이전트 레이어에서 예비 하한을 **20,000**으로 올리고, 프롬프트가 최소 `max(8_000, 0.5 × window)`를 유지하도록 캡 |
| openclaw | 서버측 `compact_threshold` = 윈도우의 **70%**, 모르면 80,000 |
| openclaw | 토큰과 무관한 두 번째 **바이트 기반** 트리거(대화록 크기) |
| Claude Code | `tokens ≥ (window − min(modelMaxOutput, 20_000)) − 13_000`. 자체 주석이 이를 **실효의 ~93%** 로 계산 |
| Claude Code | **usable의 80%** 에서 선제 사전 계산 (배포 바이너리에만 존재) |
| opencode v1 | `count ≥ limit.input − min(20_000, maxOutput)` — **usable의 100%**, 여유 없음 |
| opencode v2 | `estimate > context − max(output, 20_000)` |
| openharness | `tokens ≥ (window − 20_000) − 13_000` — Claude Code 공식의 의도적 이식 |

숫자보다 중요한 관찰 둘.

**첫째, 어느 둘도 일치하지 않고, 그 불일치에는 원칙이 있다.** hermes의 작은 윈도우 규칙 주석은, 작은
윈도우에서 50% 트리거는 회수되는 여유가 너무 적어 "compaction이 1–2턴마다 재발화하고 세션이 벽시계
시간 대부분을 요약에 쓴다"고 설명한다. Codex는 90% *트리거* 아래에 95% *usable* 윈도우를 깔아 두는데,
자기가 측정할 수 있고 게이트웨이는 측정할 수 없는 툴 오버헤드를 위해 예비를 잡아야 하기 때문이다.
이것들은 각 하네스 자신의 프롬프트 구조에 맞춰 튜닝된 값이다. 게이트웨이가 숫자 하나를 고르면 모든
호출자에 대해 틀린다.

절대 예비 규칙은 따로 떼어 볼 가치가 있다. 그것은 퍼센트 규칙과 *다른 종류*의 규칙이기 때문이다.
13,000+20,000의 고정 예비는 200K 윈도우의 ~83%이고 1M 윈도우의 ~97%다 — **실효 퍼센트가 모델에 따라
움직이며**, 이는 퍼센트 규칙과 정반대다.

Claude Code의 예비는 클라이언트 밖에서는 재현할 수 없는 방식으로 경험적으로 도출됐다:
`MAX_OUTPUT_TOKENS_FOR_SUMMARY = 20_000`에 *"compact 요약 출력의 p99.99가 17,387 토큰인 것에 근거"*
라는 주석이 달려 있다. 그것은 *자기 요약 프롬프트의* 출력 분포에 대한 측정이다. dorang에는 그런
프롬프트가 없고 따라서 그런 분포도 없다.

**둘째, 트리거는 언제나 소프트 신호일 뿐이다.** jikji가 이를 정책으로 진술한다 — *"모델 컨텍스트
윈도우를 독립적인 하드 한계로 취급하라. 캐시 정책, 쿨다운, 소프트 트리거 실패가 하드 오버플로 복구를
절대 비활성화해서는 안 된다."* 소프트 트리거와 하드 한계는 소유자가 다른 별개 메커니즘이다.
**dorang은 하드 한계를 소유한다. 소프트 트리거는 소유하지 않는다.**

### C.2 클라이언트는 실제로 무엇을 하는가

- **Codex(로컬)**: 요약 프롬프트로 추가 `/responses` 호출을 하고, 요약과 보존된 사용자 메시지로 새
  히스토리를 구성하는 **클라이언트측 히스토리 재작성**. 보존 사용자 메시지 예산 `20_000` 토큰.
- **Codex(remote v1)**: `POST /responses/compact`가 대체 히스토리를 반환하고, 그것이 다시
  **클라이언트측에서 필터링**된다 — developer 메시지, 비-사용자 컨텐트 사용자 메시지, 모든 리즈닝과
  툴 아이템, `CompactionTrigger`가 제거된다.
- **Codex(remote v2)**: in-band. `CompactionTrigger {}` 아이템을 평범한 `/responses` 요청에 밀어 넣는다.
- **Codex(토큰 예산 모드)**: **요약이 전혀 없다** — 새 컨텍스트 윈도우를 설치한다.
- **grok**: 접두부를 요약해 보존된 접두부에 이어 붙인다. 시스템 메시지가 하드 전제조건이라 그것 없이는
  compaction이 *실패*한다.
- **hermes**: 다섯 단계 — 오래된 툴 결과 정리(LLM 호출 없음) → 머리 보호 → 토큰 예산으로 꼬리 찾기 →
  중간 요약 → 이전 요약을 반복 갱신.
- **jikji**: 손실이 가장 적은 것부터의 캐스케이드 — 최근 두 라운드를 보존한 채 낡은 **툴 관측**을 제자리
  축약하고, 그다음에야 프로바이더 라운드 전체를 축출한다. 시스템 메시지는 점수 면제.
- **openclaw**: 여섯 고정 섹션(Goal, Constraints & Preferences, Progress, Key Decisions, Next Steps,
  Critical Context)으로 LLM 요약, `<previous-summary>`에 대해 반복 갱신. **toolResult 메시지는 절대
  유효한 절단점이 아니다.**
- **openclaw**: compaction 전에 별도의 **경로 결정** — `compact_only`,
  `truncate_tool_results_only`, `compact_then_truncate` — 을 예산 초과 정도로 고른다.

여섯 모두의 공통 형태: **툴 결과가 가장 먼저 희생되고, 시스템 프롬프트는 절대 아니며, 최근 꼬리는
보호된다.** 셋은 게이트웨이가 확인조차 할 수 없는 구조적 불변식을 강제한다 — hermes는 tool_call/result
그룹 내부를 절대 자르지 않고 고아 쌍을 정리해 API가 불일치 id를 보지 않게 하고, jikji는 하나의 네이티브
프로바이더 툴 배치의 부분집합 보존을 거부하며, openclaw는 마지막 N개 사용자 턴을 *그 tool-call id와
함께* 보존한다.

정확성이 아니라 보안 속성인 불변식 하나는 인용할 가치가 있다:

```
// SECURITY: toolResult.details와 런타임 컨텍스트 대화록 엔트리는 LLM 대면 compaction에 절대 들어가면 안 된다.
```

compaction은 대화를 모델에 다시 먹인다. 대화록의 어느 부분이 다시 먹여도 안전한지는 그것을 생산한
하네스가 *내용*에 대해 내리는 판단이다. 게이트웨이는 바이트를 쥐고 있고 판단은 하나도 쥐고 있지 않다.

### C.3 서버측 대 클라이언트측 — 정확한 답

둘 다 존재한다. **두 벤더에 걸쳐 서로 다른 네 가지 서버측 메커니즘**이 있고, 어느 둘도 같은 방식으로
탐지되지 않는다:

| # | 벤더 | 형태 | 탐지 수단 |
|---|---|---|---|
| 1 | OpenAI | 전용 라우트 `POST /responses/compact` | 경로 |
| 2 | OpenAI | 평범한 `/responses` 호출의 `input[]` 안 sentinel 아이템 `CompactionTrigger {}` | **요청 라인에는 아무것도 없다** — out-of-band `x-openai-subagent: compact` 뿐 |
| 3 | OpenAI | 평범한 `/responses` 호출의 `context_management: [{type: "compaction", …}]` | 본문 검사 |
| 4 | Anthropic | `/v1/messages`의 `context_management: {edits: [...]}` + beta 헤더, 응답에 에코 | 본문 **및** 헤더 검사 |

3과 4는 **필드 이름을 공유하고 그 외에는 아무것도 공유하지 않는다** — 하나는 배열, 다른 하나는 `edits`
키를 가진 객체, 서로 다른 와이어 계열, 서로 다른 의미론. 중립 표현에서 확장 필드를 이름으로 모델링하는
것에 대한 가장 강한 반증이다.

서버측 compaction 엔드포인트처럼 보이지만 아닌 다섯 번째도 있다: 어떤 하네스가 노출하는
`POST /api/session/:sessionID/compact`. 그것은 **하네스 API이지 프로바이더 API가 아니다** — 뒤에서
평범한 LLM 호출을 하고 자기 히스토리를 재작성하며 경계를 자기 DB의 행으로 영속화한다.

dorang에게 그 구별이 중요한 한 가지: **그런 하네스가 dorang 앞에 앉을 수 있다.** 그럴 때 그 compaction
호출은 요약 프롬프트를 담은 평범한 chat 요청으로 dorang에 도착한다. dorang은 알 수 없고, 알려고 해서도
안 되며, 다른 요청과 똑같이 가격 매기고 계측해야 한다. 그것은 또한 경로 안의 두 번째 compactor이며,
그게 §10.5a의 논증 그대로다 — dorang이 세 번째가 될 것이다.

그 밖에:

- **서버측 compaction은 OpenAI에서조차 프로바이더 정체성으로 게이팅된다.**
- **서버측이어도 클라이언트가 결과를 재작성한다.** 반환된 히스토리는 사용 전에 클라이언트측에서
  필터링된다. 서버측 compaction은 "서버가 대화를 소유한다"가 아니라 "서버가 후보를 만들고 클라이언트가
  결정한다"이다.
- **grok, hermes, jikji, vLLM은 전적으로 클라이언트측이다.** grok의 Responses 클라이언트는
  `previous_response_id: None`과 `store: None`을 무조건 설정한다 — 무상태이고 전체 히스토리를 다시 보낸다.
- **openclaw는 둘 다이며**, 자기 클라이언트 임계값을 서버 것 이상으로 올려 둘이 싸우지 않게 한다. 그것은
  둘 다 compaction하는 클라이언트와 서버 사이의 조정 문제다. 그 관계에 *세 번째* compactor를 끼워 넣는
  것이 §10.5a의 논증을 한 문장으로 요약한다.
- **vLLM이 제공하는 것은 compaction이 아니라 절삭이다**(§A.3a). 그 구별은 학술적이지 않다: 절삭은
  모델이 볼 수 없게 된 토큰을 버리며, 그 자리를 대신할 요약이 없다.

### C.4 왜 이 비목표가 옳은가

설계 §10.5a가 이유를 준다: 대화를 재작성하는 게이트웨이는 모델이 무엇을 요청받았는지를 바꾸고, 호출자는
응답에서 그것을 볼 수 없다. 이 조사에서 나온 세 발견이 그것을 구체화하며, 각각이 일반 논증보다 강하다.

**1. 클라이언트 자신의 안전 장치는 게이트웨이에 보이지 않는다.**

jikji는 문의가 내구적 답변을 기다리는 중일 때, 미해결 steering 폴백이 대기 중일 때, 세션 컨텍스트
일관성이 이미 불안전할 때 compaction하지 않는다. 그것들은 에이전트 루프의 상태다. 게이트웨이는 HTTP
요청을 본다. 7번 턴을 버리면 호출자가 아직 기다리는 툴 호출이 고아가 된다는 것을 알 수 없다.

이것의 가장 강한 진술은 정확히 dorang의 상황을 마주하고 거부하는 jikji 자신의 서버측 chat 경로다:

```go
// admitCompleteRequest는 정확한 프로바이더 대면 요청 사본을 검증한다. Press
// 메시지는 클라이언트 소유이므로, 이 경계는 호출자가 제공한 히스토리를 조용히
// 삭제하거나 요약하는 대신 과대 요청을 의도적으로 거부한다.
```

같은 결론에, 같은 이유로, 같은 경계에서 도달한 형제 프로젝트.

**2. compaction은 메시지의 순수 함수가 아니다 — 모델 호출과 예산이 필요하다.**

Codex, grok, hermes, jikji의 의미 경로 모두 요약을 만들기 위해 *두 번째 추론 요청*을 발행한다. grok은
그것에 32,768 토큰의 예비와 300초의 벽시계를 배정한다. dorang에게 그것은 호출자가 하나의 요청이라고
생각하는 것 안에서, 호출자가 고르지 않은 모델에, 호출자의 돈을 쓰고 — 그리고 그것을 청구한다는 뜻이다.
그것을 청구서에 정직하게 렌더링할 방법은 없다.

**3. compaction은 실패하고, 그 실패 처리는 턴에 걸쳐 상태를 갖는다.**

jikji는 영속화된 서킷 브레이커를 갖는다: 연속 세 번의 실패 *또는 무효한* 시도가 5분 쿨다운을 연다.
hermes는 두 번의 무효한 시도 후 자동 압축을 차단한다. grok은 20% 이상 줄이지 못한 compaction을 버린다.

게이트웨이라면 대화별 compaction 상태를 노드에 걸쳐 내구적으로, 그리고 자신이 갖고 있지 않은 것으로
키잉해 보유해야 한다 — chat-completions 프로토콜에는 대화 id가 없기 때문이다. 설계 §0.2는 구조적으로
단일 노드인 것이 없어야 한다고 요구한다. compaction 상태는 구조적으로 대화별이고, dorang이 주로 서빙하는
프로토콜에는 대화가 없다.

**4. 그리고 게이트웨이가 *할 수 있는* 유일한 지점이 절대 하면 안 되는 지점이다.**

vLLM의 `truncation: "auto"`는 모든 vLLM 배포에서 사용 가능하고, 필드 하나로 과대 요청을 성공시켜 줄 수
있다. 그것은 입력 길이를 클램프해 오버플로 에러가 절대 발화하지 않게 하는 방식으로 성공한다. 그것이
§10.5a가 서술하는 바로 그 실패이며, 한 줄짜리 설정 변경으로 가능하다 — 그래서 판단에 맡기는 대신 금지로
이름 붙일 가치가 있다.

### C.5 dorang은 대신 무엇을 하는가

§10.5a 그대로, §A와 §B가 확장 API 행에 힘을 실어준 채로:

| 상황 | dorang은 |
|---|---|
| 대상의 윈도우에 맞는다 | 디스패치 |
| 맞지 않고, 더 큰 윈도우의 동일 클래스 배포가 있다 | 그리로 라우팅 (§7.6 `context_window`) |
| 클래스 안 어디에도 맞지 않는다 | 실제 한계를 명시하며 실패 |
| 호출자가 벤더 자신의 compaction API를 호출한다 | 통과시킨다 (§10.6), 해석하지 않는다 |

세 번째 행이 dorang이 무언가를 더하는 지점이다. 클라이언트들은 에러 텍스트에서 실제 한계를 복구하려고
진짜 노력을 쏟는다: hermes는 에러 메시지에서 숫자를 파싱하려고 정규식 일곱 개를 들고 있고, 결과를 온전한
범위로 클램프하며, 중요하게는 **더 작은 숫자만 받아들인다** — *"컨텍스트 오버플로 복구가 새 모델 윈도우
크기를 발명해서는 안 된다"* 는 이유로. dorang은 `GET /v1/models`에서 `max_model_len`을 읽고 이미 안다.
그 숫자를 에러 본문에 넣으면 그 모든 추측을 대체한다.

---

## D. 컨텍스트 윈도우 처리 메커니즘

dorang이 절대 compaction하지 않는다면, 메커니즘 전체는 값싸게 묻고 정확히 답해야 하는 질문 하나로
환원된다: **이 요청이 맞는가?** 나머지는 전부 그 답에서 따라온다.

설계 §4.3의 비대칭이 설계를 지배한다: 윈도우가 **너무 크면** 실제 한계와 선언값 사이의 요청이 그냥
실패하고 context-window 폴백이 *절대 발화하지 않는다* — dorang이 맞는다고 믿기 때문이다. 윈도우가
**너무 작으면** 더 큰 모델로의 불필요한 라우팅, 즉 실패가 아니라 비용이다. 같은 비대칭이 *추정*에도
적용된다: **너무 낮은 추정은 폴백이 볼 수 없는 실패 모드를 만든다.** 따라서 추정기는 카탈로그와 같은
방향으로 보수적이어야 한다.

### D.1 숫자는 어디서 오는가

권위 순, 좋은 것부터:

1. **배포 자신의 선언.** vLLM/SGLang의 `GET {base}/v1/models → data[i].max_model_len`. `null`은 0이
   아니라 미선언; LoRA 카드는 `parent`에서 상속.
2. **날짜가 붙은 카탈로그 엔트리.** `context_window`와 `max_output_tokens`. 부재나 `0`은 미선언이며,
   호출자는 그것을 한계가 아니라 미지로 취급해야 한다.
3. **프리픽스 규칙.** 능력 기본값만.
4. **없음.** 미선언은 미선언으로 둔다 — 테스트가 *"미선언은 0으로 남아야 하며 절대 추측이 아니다"* 를
   고정한다.

Codex는 클라이언트 쪽에서 같은 패턴을 보여준다: 윈도우가 서버의 `/models` 테이블에서 오고, ETag에 대해
캐시되며, `X-Models-Etag`로 스트림 도중 갱신 가능하다. 그 마지막 것은 dorang에 현재 없고 채택할 수 있는
메커니즘이다 — 백엔드가 폴링 없이 카탈로그가 바뀌었다고 알려 줄 수 있다.

`POST /tokenize`도 `max_model_len`을 필수 필드로 반환해 유효한 교차 검증이지만, 토큰화 패스 비용이
들므로 프로브에 속하지 요청 경로에 속하지 않는다.

### D.2 토큰화 없는 사전 추정

설계 §15.5는 핫패스에서 리플렉션·정규식·포맷 문자열 구성을 금지하고, §7.4b는 이미 지연 근거로 토큰화를
거부했다 — VLLM.md §4가 독립적으로 확증한 거부다. 그래서 추정기는 바이트 레벨 패스여야 하고, 남는 질문은
얼마나 틀려도 되는가뿐이다.

조사한 모든 클라이언트가 토큰화가 아니라 추정을 한다: Codex `bytes/4`(올림), grok `bytes/4`(**내림** —
`estimate_tokens("abc") == 0`), hermes `chars/4` + ASCII 빠른 경로 + 조밀 코드포인트를 ~1토큰으로 세는
CJK 보정, jikji는 rune 분류(ASCII 단어 4, 문장부호 2, 공백 8 rune당 1토큰, 넓은 기호는 2),
openclaw는 하네스에서 `chars/4`에 이미지 4,800자.

jikji의 패키지 주석이 나머지가 빠지는 함정을 진술한다:

```go
// Text는 모델 가시 텍스트에 대한 보수적이고 언어 인지적인 추정치를 반환한다.
// UTF-8 바이트를 토큰으로 취급하는 것을 의도적으로 피한다: 그것은 CJK 텍스트를
// 과소 계산하면서 전송 바이트 한계를 컨텍스트 한계처럼 보이게 만든다.
```

dorang에게 이것은 결정적이다. `bytes/4`는 **CJK를 과소 계산하고**, 과소 계산은 정확히 폴백을
무력화하는 낙관적 방향이다. dorang은 한국어 배포를 서빙한다; 바이트 전용 추정기는 오버플로 가능성이 가장
높은 워크로드를 체계적으로 과소 보고한다. grok의 내림 나눗셈은 그것을 더 악화시킨다 — 모든 조각을 0
쪽으로 반올림한다.

**베낄 가치가 있는 것은 openclaw의 사전 추정기다.** 조사한 구현 중 유일하게 compaction 트리거가 아니라
admission 결정을 위해 만들어졌고, 독립적으로 아래 설계에 도달했다:

```ts
const ESTIMATED_CHARS_PER_TOKEN = 4;
const TOOL_RESULT_CHARS_PER_TOKEN = 2;
const JSON_PAYLOAD_CHARS_PER_TOKEN = 3;
const MESSAGE_BOUNDARY_OVERHEAD_TOKENS = 12;
const CONTENT_BLOCK_OVERHEAD_TOKENS = 6;
const IMAGE_BLOCK_TOKENS = 2_000;
const TRUNCATION_ROUTE_BUFFER_TOKENS = 512;
```

순진한 `chars/4`가 하지 않는 세 가지:

- **컨텐트 종류별로 다른 밀도.** 툴 결과 2자/토큰, JSON 3자/토큰은 구조화된 텍스트가 산문보다 훨씬
  조밀하게 토큰화된다는 사실을 반영한다. dorang의 트래픽은 에이전틱이므로 대부분의 바이트가 툴 결과와
  JSON이며, 평평한 제수가 가장 많이 틀리는 두 경우이고 방향은 과소 계산이다.
- **명시적 프레이밍 오버헤드** — 메시지 경계당 12토큰, 컨텐트 블록당 6토큰. jikji가 독립적으로 도달한
  같은 보정이다.
- **마지막에 적용되는 일괄 안전 마진** — `SAFETY_MARGIN = 1.2`, *"estimateTokens() 부정확성을 위한 버퍼"*.

jikji의 5% 대 openclaw의 20%라는 차이는 실제 의견 불일치이고 방향이 유익하다: jikji의 5%는 **진짜
토크나이저 위**에 있고, openclaw의 20%는 문자 휴리스틱 위에 있다. dorang은 핫패스에 토크나이저가 없으므로
20% 쪽에 가깝다 — 다만 마진은 상수가 아니라 **측정된 기본값을 가진 설정값**이어야 한다. 첫 응답의
usage가 도착하는 순간 오차가 측정 가능해지기 때문이다.

**실현 가능한 것과 비용.** dorang은 이미 prefix 라우팅을 위해 요청 본문을 스트리밍하며 4 KiB 바이트
경계에서 증분 해싱한다(§7.4b). 추정기는 같은 패스를 탄다: 256 엔트리 룩업 테이블로 ASCII 바이트를
단어·문장부호·공백 클래스로 분류하고(rune 디코드 분기 없음, 할당 없음, 정규식 없음), 비-ASCII 바이트를
따로 세되 continuation 바이트(`0b10xxxxxx`)를 건너뛰어 *코드포인트* 수를 얻고 각 비-ASCII 코드포인트에
최소 1토큰을 매긴다(바이트당 비교 하나). 메시지 경계 할증을 같은 스캔에서 더하고, 마진을 적용한다.
결과는 dorang이 이미 읽고 있는 바이트 위의 한 패스에 테이블 룩업과 카운터 둘이며, §15.5 안에 충분히 든다.

**추정은 절대 권위가 아니며, 설계는 어느 값이 이기는지 말해야 한다.** jikji의 순서가 옳다:
프로바이더 보고 usage > 프로바이더 네이티브 토크나이저 > 폴백 추정. dorang에게 그것은 추정기가
**아직 보내지 않은 요청의 admission 결정에만** 쓰인다는 뜻이다. 응답이 도착하면 §10.7의 usage 숫자가
진실이고, 추정기의 오차는 측정 가능해진다 — 추측 대신 실제 트래픽에 맞춰 튜닝 가능해진다는 뜻이다.

**적합성 의무.** 실패 방향이 비대칭이므로, 추정기에는 CJK와 긴 툴 결과 JSON과 base64 이미지 페이로드를
포함한 코퍼스에서 *과소* 추정하지 않음을 단언하는 테스트가 필요하다. hermes는 이미지당 1,600토큰을,
grok은 765를 매긴다 — 두 프로덕션 클라이언트 사이 2× 불일치이며, hermes 자신의 docstring은 상수가
1,600인데 1,500이라고 말한다. **아무도 이 숫자를 모른다.** dorang은 정당화할 수 있는 더 큰 값을 취하고
그 선택을 카탈로그에 **UNVERIFIED**로 표시해야 하며, 중간값을 고르면 안 된다.

### D.3 §7.6 `context_window` 폴백과의 관계

| 조건 | 조치 |
|---|---|
| `estimate + max_tokens ≤ 선언 윈도우` | 디스패치 |
| `>` 이고, 동일 클래스에 더 큰 선언 윈도우 배포가 있다 | 그리로 라우팅 — **400 이후가 아니라 디스패치 전에** |
| 모든 후보의 윈도우가 미선언 | 디스패치하고 400 시그니처에 의존; 해당 배포를 `UnverifiedModels()`에 기록 |
| 클래스 전체에서 한계 초과 | **`400`으로 실패**, 실제 한계·추정치·측정 대상 배포를 명시 |
| 대화가 한 계열에 고정된 opaque state를 담는다 (§B.3) | **폴백 없음**; 그 계열이 받을 수 없으면 실패 |

표가 함축하고 틀리기 쉬운 규칙 둘:

- **`max_tokens`가 윈도우에 계산된다.** vLLM은 요청마다 `max_model_len − input_length`를 계산하고
  VLLM.md §1.1은 dorang이 max-output 값을 합성하면 안 된다고 말한다. 그러므로 admission 테스트는
  호출자가 요청한 출력을 쓰고, 그것이 없을 때 숫자를 갖기 위해 큰 기본값을 발명해서는 안 된다.
- **backstop 시그니처는 유지된다.** VLLM.md §2가 그것을 고정한다: 부분 문자열
  `"maximum context length"`를 매칭하고 문장 전체는 쓰지 말 것. hermes의 28개 패턴 목록이 사전 계산 경로
  없이 어떻게 되는지 보여주며, 그 자신의 주석은 그 목록의 맨 `"max_tokens"`가 *다른* 복구 경로에 하중을
  받는다고 표시한다 — 부분 문자열 매칭이 만들어내는 결합의 예다.

#### D.3a 어떤 백엔드는 오버플로를 아예 보고하지 않는다

backstop 시그니처에는 구멍이 있고, 설계를 바꿀 만큼 크다. 한 오버플로 탐지기는 **세 경우를 갖고 그중
첫 번째만 에러다**:

- **케이스 1**: 에러 메시지 패턴.
- **케이스 2 (z.ai 스타일)**: 성공했지만 usage가 컨텍스트를 초과 — *"오버플로를 조용히 받아들이기도
  한다(usage.input > contextWindow로 탐지 가능)"*. **실제로는 오버플로인 레이트 리밋 에러**를 반환하기도
  하는데, 그러면 §7.6이 `rate_limit` 체인을 타고 동일하게 실패할 형제 배포에서 재시도한다.
- **케이스 3 (Xiaomi MiMo 스타일)**: *"입력을 contextWindow에 정확히 맞게 절삭한 뒤 output=0으로
  finish_reason 'length'를 반환한다."*
- Ollama: *"어떤 배포는 조용히 절삭하고, 어떤 배포는 에러를 반환한다."*

이것은 vLLM `truncation: "auto"`(§A.3a)와 같은 동작 부류다. §A.3a가 확립하는 것은 dorang이 그것을 절대
*요청*해서는 안 된다는 것이고, §D.3a가 확립하는 것은 **dorang이 그것이 어차피 일어나고 있지 않다고
가정할 수 없다**는 것이다.

결과 셋:

1. **`200`은 요청이 맞았다는 증거가 아니다.** dorang은 이미 계측을 위해 usage를 읽는다.
   `reported_input_tokens > 선언 컨텍스트 윈도우` 검사는 손에 든 숫자에 대한 비교 하나이고, 이 부류의
   백엔드에 대해 유일하게 가용한 신호다.
2. **`finish_reason: "length"`에 출력 토큰 0은 completion이 아니라 오버플로다.** COMPATIBILITY §4가
   종료 사유를 매핑하며, 이것은 매핑이 와이어 상으로는 맞고 의미상 틀린 경우다. 원장과
   `x-dorang-native-stop-reason`에 속한다.
3. **그것은 요청이 아니라 선언된 윈도우에 대한 증거이기도 하다.** 보고된 입력이 선언 윈도우를 넘으면
   *선언* 쪽이 틀린 것이고, 이는 설계 §4.3의 "너무 작음", 즉 양성 방향이다. dorang은 그것을 배포에 기록해
   `UnverifiedModels()`가 가리킬 것을 갖게 해야 하며, 그 증거로 선언 윈도우를 조용히 넓혀서는 **안 된다**:
   §4.3은 더 큰 값은 프로브에서만 채택될 수 있다고 명시한다.

#### D.3b 입력 오버플로와 출력 상한 오버플로는 비슷해 보이고, 체인을 공유해서는 안 된다

**입력 오버플로 400과 출력 상한 과대 400은 에러 텍스트로 구별하기 어렵다.** hermes는 두 신호 집합으로
분리한 뒤 출력 상한 경우에 빠르게 실패하며, 그 이유를 명확히 말한다:

> 그것을 압축으로 라우팅하면 같은 max_tokens를 다시 보내 동일한 400을 받고 죽음의 루프에 빠진다.

dorang의 등가 위험은 출력 상한 에러를 §7.6의 `context_window` 폴백으로 보내는 것이다: 더 큰 윈도우 모델이
같은 과대 `max_tokens`를 받고 같은 400을 반환해, `max_hops`의 모든 홉을 태우고 결국 더 느리게 실패한다.
두 원인은 §7.6에서 별도의 행이 필요하며, 출력 상한 쪽은 폴백 체인이 **없어야** 한다 — 옳은 대응은 모델의
실제 max output을 명시하는 400이고, 그 값은 dorang이 카탈로그에 갖고 있다.

### D.4 가드레일 — jikji의 가드가 막는 것

다섯 교훈, 각각 dorang 결과와 함께.

**1. 브레이커 상태는 상한이 있고 버전 검사되어야 하며, 아니면 재시작이 그것을 무기화한다.**

```go
// V1에는 영속화된 쿨다운 시작이 없어 RetryAfter에 구조적 상한이 없었다. 상한 없는
// 타임스탬프를 신뢰하는 대신 옛 선제 브레이커를 폐기한다. 오버플로 복구는 이 상태를 절대 참조하지 않는다.
```

**dorang 결과**: 재시작을 가로질러 복원하는 모든 내구 상태에 같은 것이 적용된다 — `credential_state`,
`quota_leases`, `budget_state`. 구조적 상한이 없는 영속 `unavailable_until`은 **저장된 장애**다. 복원은
검증해야 하고, 채택하는 대신 거부해야 한다.

**2. 소프트 경로의 가드가 하드 경로를 절대 비활성화해서는 안 된다.**

*"선제 시도만을 지배한다; 오버플로 복구는 그것을 절대 참조하지 않는다."* **dorang 결과**: 배포를 후보에서
제거하는 서킷 브레이커(§7.6)가 그 요청을 어차피 거부했을 capacity 검사까지 억제해서는 안 된다. 최적화
상태와 정확성 상태는 별개이며, 그 분리는 기억이 아니라 구조여야 한다.

**3. 수동 조치는 자동 브레이커를 변형하지 않고 우회한다.**

불변식 이름이 그것을 말한다: `TestManualCompactBypassesOpenOuterGuardWithoutMutatingIt`, 실패 메시지는
*"manual compact mutated automatic breaker"*. **dorang 결과**: 오퍼레이터가 쿨다운 중인 배포에 요청을
강제한다고 해서 쿨다운이 지워져서도, 연장되어서도 안 된다. §7.6의 half-open 프로브가 이미 그 형태이며,
그 테스트는 베낄 가치가 있다.

**4. 항목별 예산과 항목 간 예산은 별개 메커니즘이고 둘 다 필요하다.**

*단일* 툴 결과를 윈도우의 ~1/16로, `[8 KiB, 64 KiB]`로 클램프해 상한을 두고, 주석이 동기가 된 실패를
기록한다: *"이전에는 200k 윈도우에 100 KB였고, 그것이 결과 몇 개가 컨텍스트를 지배하게 했다."* 항목 간
누적은 컨텍스트 가드가 따로 처리하며, 관측 예산은 점유율 80% 이상에서 두 한계 모두를 잔여 바이트의
1/4로 수축시킨다. **dorang 결과**: 이것은 §5의 다축 예약, §15.4의 프로세스 전역 재생 예산과 같은
형태다 — 요청별 상한은 총량의 상한이 아니다. 독립 코드베이스에서 온, §15.4에서 이미 내린 설계 결정에
대한 직접적 지지다.

**5. 진행하는 대신 거부하고, 대신 무엇을 할지 말하라.**

```
admit provider request before call: projected input %d tokens exceeds the %d-token model
capacity and outer history could not be reduced; place large data in a file and reference
or page it instead
```

그 에러가 담은 것에 주목: 투영값, 용량, 그리고 행동. **dorang 결과**: 이것이 §C.5 세 번째 행의 템플릿이다.
dorang의 버전은 추정치, 실제 한계, 그 한계가 나온 배포, 그리고 — dorang은 갖고 있고 호출자는 갖고 있지
않으므로 — 발견한 동일 클래스 중 가장 큰 윈도우를 실어, 호출자가 다른 모델이었으면 됐을지 알 수 있게
해야 한다.

**6. 임시 override는 자동 되돌림되고, 절대 영속화되지 않으며, 클램프가 아니라 거부된다.**

과제에서 언급된 파일 중 유일하게 프로덕션 코드로 존재하는 것이 임시 노브를 영구화하지 않는 일에 관한
전부다: 턴 수와 지속 시간 상한(*"상한 있고 자동 되돌림되는 노브가 절대 무한이 되어서는 안 되며, 새
프로세스는 항상 설정된 선제 compaction 트리거로 올라온다"*), *"러너 지역, 절대 영속화되지 않는 기록"*,
범위 밖 퍼센트는 조용히 클램프되지 않고 **유효 범위를 메시지에 담아 거부**, 해체는 멱등이고 턴 경계에서
락 아래 소비되어 턴 중간 변경이 요청을 쪼갤 수 없음.

**dorang 결과**, 그리고 이것은 기존 결정 하나를 재검토하게 만든다: §10.5는 클라이언트 priority 힌트를
principal의 허용 범위로 클램프하고, §10.2는 리즈닝 effort를 선언된 레벨 집합으로 클램프한다. 클램프는
호출자가 *선호*를 표현할 때 옳다 — priority 힌트는 "허용해 주는 만큼 긴급하게"를 뜻한다. 호출자가
*요구*를 표현할 때는 틀리다. 클램프된 요구는 조용히 충족되지 않기 때문이다. 이 구별은 파라미터별로
결정하는 대신 §10.3에 진술할 가치가 있고, jikji의 규칙 — 유효 범위를 명시하며 거부 — 이 명백히 선호가
아닌 모든 것에 대한 옳은 기본값이다.

일곱 번째, 구조적: **compaction은 절대 루프해서는 안 된다.** *"RecoverOverflowOnce는 외부 투영을 한 번
compaction하고 정확히 한 번 재시도한다. 절대 루프하지 않고 실패한 재시도를 재시도하지 않는다."*
Claude Code에도 측정된 이유가 있는 같은 가드가 있다: `MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES = 3`,
주석은 *"1,279개 세션이 한 세션에서 50회 이상(최대 3,272회) 연속 실패해 전역적으로 하루 ~25만 API 호출을
낭비했다"*. dorang도 폴백에 같은 의무가 있고, §7.6은 이미 `max_hops`와 벽시계 예산으로 상한을 둔다.
§D.3b가 더하는 것은 출력 상한 400이 그 홉을 아예 소비해서는 안 된다는 점이다.

### D.5 dorang이 하면 안 되는 것

명백한 것은 §10.5a — 호출자의 대화를 조용히 변형하는 것. 여섯 가지 더, 각각 근거와 함께:

1. **호출자가 설정하지 않은 요청에 `truncation: "auto"`나 `truncate_prompt_tokens`를 절대 설정하지 말
   것**(§A.3a). 호출자가 볼 에러를 감사 불가능한 `200`으로 바꾸고, 그 배포에 대해 §7.6의
   `context_window` 탐지를 비활성화한다.
2. **그것들을 제거해서도 안 된다.** 호출자의 명시적 선택이다. 제거하면 동작하던 요청이 400이 된다.
3. **산수를 맞추려고 `max_tokens`를 합성하지 말 것.** 설계 §10.7은 필드가 필수인 Anthropic 계열로
   넘어갈 때 모델의 max output을 공급하도록 이미 요구하며 — 그것은 프로토콜 요구이고 *유일한* 경우다.
   그 밖의 모든 곳에서 VLLM.md §1.1은 dorang이 max-output 값을 합성하면 안 된다고 명시하고, §10.2는
   dorang이 호출자의 `max_tokens`를 절대 올리지 않는다고 명시한다.
4. **호출자가 보내지 않은 `prompt_cache_key`, `prompt_cache_retention`, `cache_salt`,
   `cache_control.ttl`을 절대 생성하지 말 것**(B7, B8, B20, B21). 넷 모두 호출자가 소유한 의미론을
   바꾼다 — 하나는 그의 캐시를 재분할하고, 하나는 보안 경계이며, 둘은 청구액을 바꾼다.
5. **호출자가 요청하지 않은 두 번째 추론 요청을 절대 발행하지 말 것.** 요약 없음, 핫패스의 토큰 카운팅
   왕복 없음. `POST /tokenize` 교차 검증은 프로브이지 요청 경로 단계가 아니다.
6. **추정이 주장이 되게 하지 말 것.** dorang이 추정으로 요청을 거부한다면, 에러는 그것이 추정이었다고
   말하고 추정값이 얼마였는지 말해야 한다. 그러면 동의하지 않는 호출자가 확인할 수 있다.
7. **어느 벤더 형태로든 `context_management`를 주입하거나 제거하지 말 것**(B17, B22). 그것은 *서버*에
   대한 컨텍스트 편집 지시이고, 양방향 모두 §10.5a 위반이다: 주입하면 dorang이 compaction을 결정한 당사자가
   되고, 제거하면 호출자가 마련한 편집을 조용히 비활성화한다. beta 헤더를 제거하는 것은 필드를 제거하는
   것과 같은 효과다(§A.4a).
8. **`200`을 요청이 맞았다는 증거로 취급하지 말 것**(§D.3a). 보고된 입력 토큰이 선언 윈도우를 넘으면
   기록하되, 그 증거로 선언 윈도우를 넓히지 말 것 — §4.3은 프로브에서만 윈도우 상향을 허용한다.
9. **실패 분류에 HTTP 상태만 믿지 말 것.** MiniMax는 `200` 본문 안에 에러를 반환하고, z.ai는 오버플로를
   레이트 리밋으로 보고하기도 한다. §7.6의 분류기에는 kind별 훅이 필요하며, 아니면 재시도 불가능한 것을
   재시도하고 실패한 것을 과금한다.

---

## E. 패스스루 설계 함의

### E.1 현재 상태

`internal/passthrough`는 아직 없다; 설계 §16이 계획된 레이아웃에 열거한다. §10.6이 여섯 동작과 보안
경계를 명세한다. §9.2는 이미 `responses_store(response_id, …, items, reasoning_blobs)`를 제공하고 이유를
진술한다. `internal/canonical`은 이미 `Request`, `Message`, `Block`, `Tool`에
`Extra map[string]json.RawMessage`를 싣고 chat-completions adapter가 그것을 왕복시킨다.

즉 dorang은 과제가 가정한 것보다 나은 상태다. 간극은 구체적이다.

### E.2 §10.6이 덮는 것 — 예상보다 많다

§A의 목록에 대조하면, 명세된 대로의 범용 엔진은 다음을 처리한다:

- **모든 단항 벤더 라우트**, `/responses/compact`, `/memories/trace_summarize`, `/alpha/search` 포함.
  3단계 "본문을 파싱하지 않고 중계"는 dorang이 구성할 수 없는 본문에 정확히 옳고, 5단계의 최선 노력
  계측은 `usage` 객체가 없는 compaction 응답에 대해 올바르게 degrade한다.
- **zstd 압축 요청 본문** — 파싱하지 않는 중계는 신경 쓰지 않기 때문.
- **WebSocket 라우트** — `/realtime/calls`, remote-control 계열, Responses-over-WS — 6단계로
  프레임 단위 중계.
- **보안 경계**: 매핑되지 않은 프리픽스 미서빙, traversal 거부, 프로바이더 크리덴셜이 클라이언트에 절대
  도달하지 않음. vLLM의 개발 모드 라우트(§A.3)가 이것이 구체적으로 왜 중요한지를 보여준다 —
  `/reset_prefix_cache`는 실행 중 모든 요청을 선점할 수 있고, `/v1/chat/completions`와 같은 base URL 위에
  있다. **`/vllm`을 여는 프리픽스 맵은 그것도 연다.**

### E.3 필요한데 없는 것

구현 순서로 일곱 항목.

**E3.1 — 라우트 맵은 프리픽스가 아니라 허용목록이어야 한다.** §10.6의 설정은 프리픽스를 프로바이더에
매핑하고 나머지 경로를 결합한다. §A.3을 보면 그것은 `/reset_prefix_cache`, `/collective_rpc`,
weight-update 계열로의 열린 문이다. 엔진에는 기본 닫힘의 라우트별 method+path 허용목록이 필요하다:

```yaml
passthrough:
  routes:
    - prefix: /vllm
      provider: vllm-local
      allow:
        - { method: POST, path: /tokenize }
        - { method: POST, path: "/v1/responses/*/cancel" }
      # 그 밖은 전부 404; /reset_prefix_cache에 도달 불가
```

§10.6은 이미 "매핑되지 않은 프리픽스는 서빙되지 않는다 — 이것은 오픈 프록시가 아니다"라고 말한다. 이것은
같은 진술을 한 단계 아래에서 참으로 만들며, 파괴적 라우트가 사는 곳이 바로 그 아래다.

**E3.2 — 헤더 정책은 2분류가 아니라 3분류여야 한다.** §10.6 4단계는 "dorang 소유 헤더만 교체; hop-by-hop
헤더 제거"라 하고 COMPATIBILITY 7.3은 여섯 인증 헤더 제거를 요구한다. 그러면 `x-codex-turn-state`(B4),
`x-grok-conv-id`와 동류, `x-codex-beta-features`, `ChatGPT-Account-ID`가 미분류로 남는다:

| 분류 | 처리 | 예 |
|---|---|---|
| dorang 소유 | 교체 | `x-dorang-*`, `X-Request-Id` |
| 크리덴셜 보유 | **항상 제거** | COMPATIBILITY 7.3의 여섯, `ChatGPT-Account-ID` |
| hop-by-hop | 제거 | `Connection`, `Transfer-Encoding` |
| **벤더 opaque** | **그대로 전달** | `x-codex-turn-state`, `x-codex-turn-metadata`, `x-openai-subagent`, `-model-override`를 제외한 `x-grok-*`, `OpenAI-Beta`, `anthropic-beta`, `chatgpt-account-id`, `originator`, `session_id` |
| gateway 소유 | dorang이 설정, 클라이언트 값 폐기 | `x-grok-model-override` (§A.2) |

벤더 opaque 분류는 프로바이더 kind별 설정이어야 한다. 이름들이 그렇기 때문이다.

**E3.2a — 어떤 헤더와 본문 필드는 한 단위이고 함께 움직여야 한다.** `anthropic-beta:
context-management-2025-06-27`가 `context_management`가 본문에 실릴 수 있는지 자체를 게이팅하고,
`cache_edits`에도 같은 패턴이 성립한다. 독립적으로 도는 헤더 필터와 본문 필터는 쌍의 한쪽을 떨어뜨릴 수
있다. 패스스루와 adapter 경로 모두 **결합 쌍 표**가 필요하다: 어느 한쪽을 버리면 둘 다 버려야 하고, 둘 다
버리는 것은 조용한 손실이 아니라 보고 가능한 손실인 `(header, body_field)` 쌍들.

새 메커니즘이 아니다 — COMPATIBILITY 5.3의 툴 이름 매핑과 같은 형태다. 다만 오늘 두 절반이 서로 다른
코드에서 처리되므로 말해 둘 필요가 있다.

**E3.3 — 상태성은 패스스루 경로가 아니라 *adapter* 경로에 있다.** OpenAI의 세 서버측 compaction
메커니즘 중 둘이 평범한 `/responses` 호출에 탄다(§C.3). 호출자가 패스스루 프리픽스가 아니라 dorang 자신의
adapter로 `/v1/responses`에 도달하면 dorang은 본문을 디코드하고, 그러면 반드시:

- 알 수 없는 `input[]` 아이템 타입을 거부하는 대신 보존해야 하고(B3);
- 값이 중첩 배열이나 객체인 알 수 없는 **최상위** 본문 필드를 보존해야 한다(B17). `canonical.Request.Extra`가
  `map[string]json.RawMessage`이므로 형태는 이미 지원되며, 없는 것은 그것이 Responses adapter를 살아남는다는
  증명이다 — 그 adapter가 아직 존재하지 않는다;
- `CompactionTrigger`를 히스토리로 `responses_store`에 영속화해서는 안 되고 — 그것은 요청 제어다;
- 반환된 `Compaction { encrypted_content }` 아이템을 response id로 키잉해 `reasoning_blobs`나 형제 컬럼에
  그대로 저장해야 한다.

`canonical.Block`에는 이미 `Extra`와 `Kind`가 있으므로 알 수 없는 아이템 타입이 살 곳은 있다. 없는 것은
*알 수 없는 아이템 종류는 버리지 않고 보존한다*는 디코드 규칙과, `CompactionTrigger {}` 아이템과
`context_management` 배열이 디코드/인코드 왕복을 변형 없이 살아남는다는 골든 테스트다.

**E3.3a — `store`는 이제 세 방향으로 하중을 받는다.** 설계 §10.7은 `Store`를 평범한 Responses 계열 필드로
열거한다. 아니다: 어떤 클라이언트의 서버측 compaction과 vLLM의 `background` 모드에는 `store: true`가
**필수**이고, Codex 라우트는 그것을 **거부**하며(*"Store must be set to false"*), vLLM은 기본값을
**`true`** 로 두는데 Codex는 Azure를 제외하면 `false`로 둔다.

즉 `store`를 버리거나 기본값을 주거나 정규화하는 것은 같은 와이어 계열의 서로 다른 백엔드에서 반대 방향으로
동작을 바꾼다. §10.3의 범용 파라미터 필터가 아니라 kind별 능력 표에 **3상태**(required / forbidden /
free)로 속한다.

**E3.4 — `responses_store`에 컬럼 둘이 더 필요하다.** §9.2에는 `items`와 `reasoning_blobs`가 있다. §B가
더한다: **`family_pin`** — 이 대화의 opaque state를 발행한 `(kind, model-family)`, 그래야 §B.3의 고정
규칙이 희망이 아니라 강제 가능해진다. **`opaque_headers`** — `x-codex-turn-state`와 동류의 값. 그것들은
턴별이고 *응답*에 도착하지만 *다음 요청*에 나가야 하며, 오늘 둘 곳이 없다.

**E3.5 — 능력 광고에 opaque-state 축이 필요하다.** `internal/canonical/capability.go`에는 `Structural`과
`Droppable`이 있다. 어느 쪽도 "이 배포는 계열 X가 발행한 암호화된 리즈닝 컨텐트를 받을 수 있다"를
표현하지 못한다. 최소한의 추가는 벤더별 비트가 아니라 — 그것은 확장되지 않는다 — 프로바이더 설정의
배포별 `opaque_state_realm` 문자열이다: 한쪽이 발행한 state를 다른 쪽이 받아들이면 둘은 같은 realm이다.
라우팅은 요청이 opaque state를 실을 때 realm 동일성으로 필터하고, §7.6은 realm을 가로지르는
`context_window` 폴백을 억제한다. §B.3의 제약을 확인 가능하게 만드는 가장 작은 것이다.

**E3.6 — 계측이 파싱을 요구해서는 안 된다.** §10.6 5단계는 이미 계측 실패가 요청을 실패시키지 않는다고
말한다. 더할 것: compaction 호출에서 본문은 *대화*이므로 요청이 크고 응답에는 `usage`가 없다. 카운트와
바이트를 기록하는 것이 옳은 결과이고, 원장 행에 `route_kind: passthrough` 마커를 실어 지출 리포트가 정체
모를 0토큰 요청을 보여주지 않게 해야 한다.

**E3.7 — 호출자에 대한 능력 광고.** 호출자는 어떤 벤더 라우트가 열려 있는지 발견할 수 없다.
COMPATIBILITY §9는 미구현 라우트가 조용한 `404`가 아니라 기계 판독 가능한 사유와 함께 `501`을 답해야 한다고
말한다. 패스스루도 같아야 하고, 열린 프리픽스와 그 허용목록을 열거하는 `GET /v1/dorang/capabilities`(또는
관리 표면의 등가)는 비용이 적고 "내 compaction 호출이 왜 404인가" 부류의 문의를 통째로 없앤다.

### E.4 §10.6이 명시적으로 자라면 *안 되는* 것 하나

compaction 응답을 *해석*하는 법을 배워서는 안 된다. 반환된 히스토리를 재작성하거나, canonical 메시지로
저장하거나, 다른 백엔드에 재발행하는 것은 전부 패스스루를 명세가 없고 예고 없이 바뀌는 사유 프로토콜의
adapter로 바꾼다. §10.5a의 경계가 옳고 §10.6의 "본문을 파싱하지 않고 중계"가 이미 그것을 강제한다.
부수효과로 남기지 말고 의도로 진술해야 한다.

---

## F. 순위 매긴 권고

틀렸을 때의 비용 순.

1. **MUST — dorang은 `truncation` / `truncate_prompt_tokens`를 절대 설정하거나 제거하지 않는다.**
   설정하면 감사 가능한 에러를 조용한 `200`으로 바꾸고 그 배포에 대해 §7.6의 `context_window` 탐지를
   비활성화한다. 제거하면 호출자의 명시적 선택을 깨뜨린다. §4.3이 프리픽스 규칙에 리즈닝을 거부하는 것과
   같은 방식으로, 설정 로드 시 거부로 강제할 것. → §A.3a, §D.5.1–2

2. **MUST — `context_management`를 계열별 opaque로 취급하고 beta 헤더와 함께 움직일 것.**
   이름을 두 벤더가 호환되지 않는 스키마로 공유하므로 `CanonicalRequest`에서 이름으로 모델링해서는 절대
   안 된다. Anthropic 쪽은 beta 헤더로 게이팅되고 응답에 에코된다. 이것이 2위인 이유는 심각도가 낮아서가
   아니라 1번보다 범위가 좁아서다: T0 경로인 `/v1/messages`에 착지하고 §6이 그것을 passthrough가 아니라
   adapter라고 선언하므로, 그것을 실을 수 없는 변환에는 조용한 생략이 아니라 §10.1에 따른 construct id와
   `400`이 필요하다. → §A.4a, §B.2 B22–B23, §E3.2a, §E3.3

3. **MUST — 패스스루 라우트는 기본 닫힘의 method+path 허용목록이다.** vLLM의 개발 모드 계열이 추론과 같은
   base URL에 있고 `/reset_prefix_cache`는 실행 중 모든 요청을 선점할 수 있다. 프리픽스 전용 맵은 그것들을
   연다. → §E3.1

4. **MUST — 헤더를 둘이 아니라 넷으로 분류하고 벤더 opaque 분류는 그대로 전달할 것.**
   `x-codex-turn-state`는 응답에서 설정되어 다음 요청에 에코된다. 일괄 인바운드 헤더 제거는 그것을
   깨뜨리고, 일괄 전달은 크리덴셜을 흘린다. 헤더 필터와 본문 필터는 결합 쌍 표도 공유해야 한다. → §E3.2, §E3.2a

5. **MUST — opaque state를 실은 대화를 호환 배포 계열에 고정하고, 계열을 가로지르는 `context_window`
   폴백을 억제할 것.** 교차 계열 사례는 절대 재시도 불가로 분류된 하드 400이므로 폴백이 구할 수 없다 —
   모든 홉을 태우고 같은 에러를 반환한다. → §B.3, §D.3, §E3.5

6. **MUST — 추정기는 과소 계산해서는 안 되고 바이트 전용이어서도 안 된다.** `bytes/4`는 CJK를 과소
   계산하고, 과소 계산은 폴백을 발화하지 못하게 만드는 방향이다. 기존 §7.4b 바이트 스캔에 컨텐트 클래스
   카운팅을 접어 넣고, 메시지와 컨텐트 블록당 프레이밍 할증과 마진을 둘 것. CJK + 툴 JSON + 이미지
   코퍼스에서 과소 계산하지 않음을 단언하는 적합성 테스트를 추가할 것. → §D.2

7. **MUST — `store`를 droppable 파라미터가 아니라 kind별 3상태로 만들 것.** 어떤 곳에서는 *필수*,
   Codex 라우트에서는 *거부*, 같은 와이어 계열의 백엔드들에서 기본값이 반대 방향. → §E3.3a

8. **MUST — §7.6에서 출력 상한 400을 입력 오버플로 400과 분리하고, 출력 상한 원인에는 폴백 체인을 주지
   말 것.** 출력 상한 에러를 `context_window` 폴백으로 라우팅하면 같은 과대 `max_tokens`를 더 큰 모델에
   다시 보내 동일한 400을 얻는다. → §D.3b

9. **MUST — 한계 초과 에러는 실제 한계, 추정치, 배포를 명시할 것.** 이것이 §10.5a 세 번째 행의 존재
   이유 전부다. 그러지 않으면 클라이언트가 에러 산문에서 정규식 일곱 개로 파싱하고 자기가 가진 것보다
   큰 어떤 숫자도 믿기를 거부한다. → §C.5, §D.4.5

10. **MUST — 호출자가 보내지 않은 `prompt_cache_key`, `prompt_cache_retention`, `cache_salt`를 절대
    생성하지 말고, 보낸 것을 절대 버리지 말 것.** 키들은 호출자가 관리하던 캐시를 조용히 재분할하고 §8의
    비용 엔진을 오산하게 한다; `cache_salt`는 보안 경계이고; retention은 과금되는 정책이다.
    → §B.2 B7–B8, B20–B21, §D.5.4

11. **MUST — `/v1/models`는 업스트림 `ETag`를 전달해서는 안 된다.** dorang은 호출 키의 허용목록에 따라
    모델 목록을 재작성하므로(COMPATIBILITY 7.4), 업스트림 validator는 dorang이 보내지 않은 본문을
    validate하게 된다. 자체 ETag를 발행하거나 아무것도 보내지 말 것. → §B.2 B15

12. **MUST — `responses_store`에 `family_pin`과 `opaque_headers`를 추가할 것.** 없으면 권고 4와 5가
    상태를 둘 곳이 없고, 턴별 opaque 헤더가 응답에 도착해 다음 요청까지 살아남을 곳이 없다. → §E3.4

13. **SHOULD — usage 확장을 재계산으로 덮지 말고 보존할 것.** grok이 `context_details`에서
    `total_tokens`를 의도적으로 다시 쓰는 이유는 서버측 툴 루프 아래에서 누적값이 틀리기 때문이고, 그
    숫자가 클라이언트의 compaction 트리거를 구동한다. §10.7의 정규화는 덮어쓰는 게 아니라 더해야 한다.
    `cost_in_usd_ticks`도 읽을 것 — 벤더가 이미 요청 가격을 매겼다. → §A.2a, §B.2 B12–B13

14. **SHOULD — 알 수 없는 `input[]` 아이템 종류를 디코드/인코드를 통해 보존하고, `CompactionTrigger {}`
    를 쓰는 골든 테스트를 둘 것.** remote compaction v2는 탐지할 라우트도 화이트리스트할 필드도 없다;
    평범한 요청 위의 sentinel 아이템이다. 검증하는 adapter는 그것을 거부한다. → §E3.3

15. **SHOULD — §2.1 라우트 표를 백엔드가 실제로 서빙하는 것과 조정할 것.** vLLM은 §2.1에 없는
    `POST /v1/responses/{id}/cancel`을 구현하고, §2.1에 있는 `DELETE /v1/responses/{id}`를 구현하지
    않는다. `GET`은 `starting_after`와 `stream`도 받는다. → §A.3

16. **SHOULD — 제공하는 kind에 대해 `X-Models-Etag` 스타일 카탈로그 무효화를 채택할 것.** 백엔드가
    폴링 없이 추론 스트림에서 모델 테이블이 바뀌었다고 신호할 수 있다. dorang에는 현재 갱신 신호가 전혀
    없고 §4.3의 `UnverifiedModels()` 목록은 손으로만 자란다. → §D.1

17. **SHOULD — 내구 상태를 복원 시 검증하고 불일치 시 채택 대신 리셋할 것.** 영속화된 타임스탬프에
    구조적 상한이 없다는 이유로 브레이커 버전 하나를 통째로 폐기한 선례가 있다. 같은 것이
    `credential_state.unavailable_until`, `quota_leases`, `budget_state`에 적용된다. → §D.4.1

18. **SHOULD — 소프트 경로 브레이커가 하드 경로 검사를 절대 억제하지 않게 할 것.** dorang에게: 배포
    서킷 브레이커가 어차피 그 요청을 거부했을 capacity admission을 억제해서는 안 된다. 기억이 아니라
    구조로 만들 것. → §D.4.2

19. **SHOULD — 패스스루를 `route_kind` 마커와 함께 계측하고 토큰 없는 행을 받아들일 것.** compaction
    호출은 요청 본문이 거대하고 응답에 `usage`가 없다. 설명 없는 0토큰 요청이 찍힌 지출 리포트는 곧
    문의다. → §E3.6

20. **SHOULD — 어떤 패스스루 라우트가 열려 있는지 광고하고 나머지는 `501`로 답할 것.**
    COMPATIBILITY §9는 이미 조용한 `404`보다 기계 판독 가능한 사유의 `501`을 요구한다; 패스스루가
    예외일 이유는 없다. → §E3.7

21. **SHOULD — 지원 집합과 별개로 kind별 거부 파라미터 목록을 유지할 것.** §10.3의 `drop_unsupported`는
    kind의 지원 집합에 대해 필터한다. Codex 라우트에는 그 역이 필요하다: *같은 벤더, 같은 와이어 계열*이
    다른 크리덴셜로 도달했을 때 거부하는 파라미터 목록. dorang의 `codex-responses` kind가 옳은 자리다.
    → §A.1b

22. **SHOULD — 여기서 발견된 벤더 네이티브 종료값에 대해 `finish_reason` 행을 추가할 것.**
    COMPATIBILITY §4.1의 목록에 `abort`, `repetition`(VLLM.md §2.3), z.ai의
    `model_context_window_exceeded`가 없다. 4.2에 따라 매핑되지 않은 값은 `"stop"`이 되고 경고를 남기는데,
    마지막 것에 대해서는 오버플로를 정상 턴 종료로 보고하는 셈이다. → §A.4

23. **SHOULD — 배포별 `overflow_behavior` 능력을 기록할 것.** 백엔드마다 과대 요청이 에러가 되는지,
    조용히 절삭되는지, 레이트 리밋 에러를 반환하는지가 다르다. §4.3과 VLLM.md §1.2가 미검증 능력에 이미
    쓰는 패턴 그대로다: 오퍼레이터 선언, 기본은 미지, 그리고 §D.3a의 usage 기반 검사를 런타임 backstop으로.
    → §D.3a

---

## G. 미검증

가정하지 않고 기록한다.

- **`/responses/compact`가 `chatgpt.com/backend-api/codex`가 아니라 `api.openai.com`에도 존재하는지.**
  Codex는 auth 모드로 base URL을 고르고 remote compaction을 프로바이더 정체성으로 게이팅하는데, 시사적이지만
  증명은 아니다. 라이브 호출은 하지 않았다.
- **Rust 구조체 너머의 `/responses/compact` 요청 본문 와이어 스키마.** 클라이언트 관점만 읽었다. 서버가
  받아들이는 스키마는 이 트리에서 관찰 불가능하다.
- **`Compaction.encrypted_content`의 내용.** 구성상 opaque다. Codex 트리 어디에서도 검증하거나 들여다보지
  않는다.
- **이미지당 토큰 비용.** 1,600 대 765, 그리고 한 클라이언트 자신의 docstring은 1,500이라고 말한다. 숫자
  셋, 클라이언트 둘, 자기모순 하나.
- **vLLM의 `truncation: "auto"`가 실제로 왼쪽에서 절삭하는지 오른쪽에서 절삭하는지.** `truncation_side`가
  기본 `None`이고 토크나이저 기본값으로 폴백하는데, 그 기본값은 읽지 않았다.
- **Claude Code 인용은 서로 완전히 일치하지 않는 두 산출물에서 왔고 둘 다 조건부다.** 하나는 날짜가 붙은
  유출 TypeScript 스냅샷이고, 다른 하나는 JS가 한 줄로 minify된 컴파일 단일 파일 실행 파일이라 "줄 번호"가
  의미 없고 인용된 문자열만 의미가 있다. 둘이 일치하는 곳에서 소스를 인용했다. 두 기능은 바이너리에만
  나타나므로 유출 소스는 최신이 아니다. Claude Code의 모든 수치는 공표된 계약이 아니라 *이 기계에서 관찰된
  것*으로 취급할 것.
- **Anthropic의 `context_management`가 일반 가용인지.** 툴 클리어링 전략은 그것을 만드는 코드에서 내부
  사용자로 게이팅된다. 공개 배포는 그것을 절대 내보내지 않을 수도 있다. 그것은 중계할 이유이지 모델링할
  이유가 아니다.
- **`clear_tool_uses_20250919` / `clear_thinking_20251015`의 서버측 의미론.** 클라이언트의 타입 정의만
  읽었다. 서버가 `clear_at_least`나 `exclude_tools`로 무엇을 하는지는 관찰하지 않았다.
- **§D.2 추정기의 측정된 정확도.** 명세됐을 뿐 만들어지지 않았고, 마진은 진짜 토크나이저 위의 5%와 휴리스틱
  위의 20% 사이에 괄호로 묶여 있을 뿐 dorang 자신의 트래픽에 대해 측정되지 않았다.
