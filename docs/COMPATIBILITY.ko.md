# 와이어 호환 계약

> 클라이언트가 실제로 관찰하는 것, 따라서 dorang이 바이트 단위로 재현해야 하는 것.
> 여기의 모든 항목은 참고사항이 아니라 **골든 테스트**다.
>
> 출처: OpenAI·Anthropic 공개 와이어 포맷, 그리고 기존 클라이언트 대부분이 대상으로 삼아
> 만들어진 널리 배포된 OpenAI 호환 Python 프록시의 실측 동작. 둘이 어긋나는 곳은 dorang이
> 어느 쪽을 따르는지와 이유를 명시한다.
>
> English (기준 문서): [COMPATIBILITY.md](COMPATIBILITY.md)

---

## 0. 표면 규모

505개 경로를 가진 실제 배포를 실제로 붙어 있는 클라이언트와 대조 감사한 결과:

| 티어 | 경로 | 오퍼레이션 | 의미 |
|---|---:|---:|---|
| T0 | 11 | 14 | 붙어 있는 클라이언트가 즉시 깨진다 |
| T1 | 88 | 123 | 범용 SDK 호출이 실패한다 |
| T2 | 343 | 419 | 컨트롤 플레인·엔터프라이즈 기능 |
| T3 | 63 | 139 | 폐기·UI 내부·벤더 패스스루 |

**"아무것도 안 깨지는" 지점이 표면의 2.2%. 방어 가능한 전체 추론 프로토콜(T0+T1)이 19.6%.**
나머지 80%는 컨트롤 플레인이다. 설계 §0.3이 추론 프로토콜을 전량 구현하고 관리 표면을 단계적으로
채우는 이유는 취향이 아니라 이 비율이다.

### T0 — 먼저 구현

`POST /v1/chat/completions` · `POST /chat/completions` · `POST /v1/embeddings` ·
`POST /embeddings` · `GET /v1/models` · `GET /models` ·
`GET /health/liveliness` · `/health/liveness` · `/health/readiness` (GET **및** OPTIONS) ·
`POST /v1/messages` · `POST /v1/messages/count_tokens`

---

## 1. chat completions — SSE 프레이밍

| # | 계약 |
|---|---|
| 1.1 | 각 청크는 정확히 `data: <json>\n\n`. `event:` 줄 없음, `id:` 줄 없음. |
| 1.2 | 스트림은 `data: [DONE]\n\n` 로 종료. |
| 1.3 | 스트림 도중 오류는 **인밴드**로 전달: `data: {"error":{…}}\n\n` 다음 `data: [DONE]\n\n`. 첫 청크가 이미 나갔으면 HTTP 상태는 이미 200이고 바꿀 수 없다 — 설계 §7.6이 폴백으로 넘지 않겠다고 한 경계가 정확히 이것이다. |
| 1.4 | 업스트림 스트림이 비면 본문이 비고 `[DONE]`도 **없다**. |
| 1.5 | 업스트림 SSE 원시 줄(`data:`, `event:`, `:`)은 그대로 통과. 구분자가 빠진 프레임만 재프레이밍. |

## 2. chat completions — 청크 JSON 형태

| # | 계약 | 왜 문제가 되는가 |
|---|---|---|
| 2.1 | **없는 필드는 생략되며, 절대 `null`로 나가지 않는다.** | 참조 직렬화기는 unset과 none을 제외한다. `omitempty`가 없는 Go 구조체는 `"logprobs":null,"system_fingerprint":null`을 내보내 모든 기존 클라이언트의 기대와 어긋난다. **자기 테스트는 통과하면서 호환성을 깨는 가장 쉬운 방법**이다. |
| 2.2 | 평범한 텍스트 청크는 정확히 `{id, object, created, model, choices:[{index, delta:{role?,content?}, finish_reason?}]}`. 그 이상 없음. | 키가 더 있으면 그것이 어긋남이다. |
| 2.3 | `object`는 항상 `"chat.completion.chunk"`. | |
| 2.4 | `id`와 `created`는 한 스트림의 **모든 청크에 고정**된다. | 청크마다 `created`를 새로 만들면 그것을 스트림 식별자로 쓰는 클라이언트가 깨진다. |
| 2.5 | `model`은 **모든 청크에서** 클라이언트 대면 이름으로 다시 찍힌다. | 설계 §7.2를 독립적으로 확증한다: 프레임별 재작성은 피할 최적화가 아니라 요구되는 동작이다. 개정 1의 "첫 프레임 오프셋 패치"는 이 근거만으로도 틀렸다. |

## 3. usage

| # | 계약 |
|---|---|
| 3.1 | usage 청크는 `stream_options.include_usage`가 정확히 `true`일 때 **만** 나간다. truthy로는 부족하다. |
| 3.2 | `stream_options`가 없으면 usage는 계산되지만 **와이어에 나가지 않는다**. |
| 3.3 | ⚠️ **OpenAI와의 차이.** 참조 프록시의 usage 청크는 `"choices":[{"index":0,"delta":{}}]`를 담고, OpenAI는 `"choices": []`를 보낸다. dorang은 **참조 프록시를 따른다** — 기존 클라이언트가 그것을 대상으로 만들어졌기 때문이다. 엄격한 OpenAI 형태를 원하는 호출자를 위해 `compat.usage_chunk_choices: empty`를 제공한다. |
| 3.4 | 일부 백엔드는 `finish_reason` **이후에** usage를 보낸다. 누산기는 finish에서 닫지 말고 늦은 usage를 받아야 한다. |
| 3.5 | usage는 비-OpenAI 확장(비용, 캐시 토큰 상세, 서버 툴 사용)을 실을 수 있다. dorang은 자기 것을 `x-dorang-*` 헤더로 내보내고, 널리 읽히는 usage 확장 필드는 호환을 위해 미러링한다. |

## 4. `finish_reason`

| # | 계약 |
|---|---|
| 4.1 | 백엔드 고유 값은 최소한 다음을 포함하는 매핑 표를 거친다: `end_turn`, `stop_sequence`, `max_tokens`, `tool_use`, `refusal`, `COMPLETE`, `ERROR`, `ERROR_TOXIC`, `eos_token`, `eos`, `STOP`, `SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII`, `IMAGE_SAFETY`, `network_error`, `sensitive`, `guardrail_intervened`. |
| 4.2 | 매핑되지 않은 값은 `"stop"`이 되며 **경고를 남긴다**. 원본 그대로 통과시키지 않는다. |
| 4.3 | 원본 값은 별도 필드에 보존되므로 실제로 잃는 것은 없다. |
| 4.4 | ⚠️ **백엔드가 종료 청크를 보내지 않으면 합성한다.** 기본 `"stop"`, **그리고 스트림에서 툴 호출이 관찰됐으면 `"tool_calls"`로 승격**한다. 그냥 전달만 하는 게이트웨이는 툴 호출 턴에 `"stop"`을 내보내 모든 에이전틱 클라이언트를 깨뜨린다. **이 문서에서 가치가 가장 높은 한 줄이다.** |

## 5. 툴 호출 스트리밍

| # | 계약 |
|---|---|
| 5.1 | `delta.tool_calls[].index`는 **필수**이며 optional이 아니다. `id`, `type`, `function`이 optional이다. |
| 5.2 | 인바운드 assistant 메시지는 전달 전에 `tool_calls`에서 `index`가 **제거**되므로, 전체 assistant 메시지를 되돌리는 클라이언트가 거부되지 않는다. |
| 5.3 | 툴 이름은 OpenAI 쪽에서 64자로 제한된다. 절삭은 매핑에 기록하고 **왕복**해야 하며, 아니면 모델의 툴 호출을 되짚을 수 없다. |
| 5.4 | 프로토콜 간 tool-use id는 양방향으로 일관되게 정규화해야 한다. |

## 6. `/v1/messages` — 가장 위험한 표면

Anthropic 형태 요청이 OpenAI 형태 백엔드로 가는 일이 매우 흔하므로, 이것은 패스스루가 아니라
어댑터다.

| # | 계약 |
|---|---|
| 6.1 | SSE 프레이밍이 `event: <type>\ndata: <json>\n\n` — chat completions와 달리 **두 줄**이다. |
| 6.2 | 이벤트 타입: `message_start`, `content_block_start`, `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`. **`ping` 없음. `[DONE]` 없음.** `message_stop`은 정확히 한 번. |
| 6.3 | 어댑터 경로의 컨텐트 블록 타입: `text`, `tool_use`, `thinking`. |
| 6.4 | ⚠️ `stop_reason`이 세 값으로 붕괴한다: `stop→end_turn`, `length→max_tokens`, `tool_calls→tool_use`, **그 외 전부 → `end_turn`**. 즉 `content_filter`가 정상 종료로 보고된다. dorang은 호환을 위해 이를 재현하고 **동시에** 실제 사유를 `x-dorang-native-stop-reason`으로 알린다 — 클라이언트를 깨지 않으면서 정보는 남긴다. |
| 6.5 | 어댑터 경로에서 `stop_sequence`는 `null`. |
| 6.6 | 컨텐트 블록 인덱싱은 **상태를 갖는다**: 보류된 `message_delta`, 청크 큐, 블록 전환 시 합성되는 `content_block_stop` → `content_block_start` 쌍. `content_block_stop`은 항상 마지막 `message_delta`보다 먼저 나가야 한다. 게이트웨이 전체에서 가장 복잡한 상태 기계이며 별도 퍼즈 타깃을 갖는다. |
| 6.7 | 프롬프트 캐싱: `message_start`가 캐시 카운터를 0으로 심고, 마지막 `message_delta`가 실제 값을 채운다. 캐시 필드는 **0보다 클 때만** 나타난다. `input_tokens = prompt − cache_read − cache_creation`(0에서 클램프). 백엔드가 OpenAI 관례를 쓰면 cache_read는 그쪽 필드로 폴백한다. |
| 6.8 | ⚠️ 비스트리밍 응답은 **비표준 `usage.total_tokens`** 를 포함하는데 스트리밍은 생략한다. 두 형태가 필드 하나만큼 다르다. dorang은 기본적으로 `compat.anthropic_total_tokens: true`로 이 비대칭을 재현한다. |
| 6.9 | `/v1/messages/count_tokens`는 정확히 `{"input_tokens": <숫자>}` 를 반환하고 `?beta=true`를 받는다. |

## 7. 전 영역 공통

| # | 계약 |
|---|---|
| 7.1 | 오류 봉투: `{"error":{"message":str,"type":str,"param":str\|null,"code":"<문자열>"}}`. **`code`는 숫자가 아니라 문자열이다.** |
| 7.2 | "가용 배포 없음" 조건은 503이 아니라 **429**로. 태그 라우팅 미스는 **401**로 매핑된다. |
| 7.3 | **여섯 가지 인증 헤더 이름**을 받으며 그중 아무거나로 인증된다: `Authorization: Bearer`, `API-Key`, `x-api-key`, `x-goog-api-key`, `Ocp-Apim-Subscription-Key`, 그리고 프록시 고유 헤더. dorang은 여섯 개를 모두 받고 **업스트림으로 전달하기 전에 전부 제거한다.** |
| 7.4 | `GET /v1/models` 항목은 `{"id","object":"model","created":<상수>,"owned_by"}`. `created`는 현재 시각이 아니라 **고정 상수** — 클라이언트가 그것으로 캐싱한다. 목록은 호출 키의 모델 허용목록으로 필터링된다. |
| 7.5 | ⚠️ **라우트 매칭은 구체성 순서다.** `/openai/deployments/{model}/chat/completions`가 `/openai/{endpoint...}`보다 먼저 매칭돼야 한다. 단순 프리픽스 라우터는 구체적 라우트를 catch-all에 조용히 삼킨다. |
| 7.6 | 미지원 파라미터는 기본적으로 **조용히 드롭**되며 거부되지 않는다. 전부 그대로 전달하는 게이트웨이는 클라이언트가 본 적 없는 업스트림 400을 노출시킨다. dorang이 `drop_unsupported: true`를 기본값으로 두는 이유다. |
| 7.7 | 응답 헤더를 읽는 도구가 있다 — 특히 call id와 모델/배포 id. dorang은 `x-dorang-request-id`와 `x-dorang-deployment`를 내보내고, `compat.legacy_headers`가 켜지면 널리 읽히는 레거시 헤더 이름을 미러링한다. |
| 7.8 | 클라이언트가 인바운드 call-id 헤더를 설정하면 그것을 요청 id로 존중한다. |

## 8. 라우팅 동작도 호환성의 일부다

사용자에게는 신뢰성 회귀와 프로토콜 파손이 구별되지 않는다. 비교 대상 배포가 실제로 쓰는
기본값이자 dorang이 표현할 수 있어야 하는 것:

```
strategy: least_busy
num_retries: 2
cooldown: 5s          연속 3회 실패 후
timeout: 6000s
```

핵심은 cooldown이다. 모델 이름 하나에 배포가 여럿일 때, 실패하기 시작한 백엔드가 선택에서
빠져야 한다. cooldown 없는 라운드로빈은 계속 절반의 트래픽을 보내며 게이트웨이 버그처럼 보인다.

## 9. 의도적으로 구현하지 않는 것

| 영역 | 이유 |
|---|---|
| 벤더 패스스루 catch-all | 감사한 배포에서 그중 어떤 것에도 프로바이더 크리덴셜이 **없었고** 모든 배포에서 패스스루가 비활성이었다. dorang은 설정으로 켤 수 있도록 범용 엔진(설계 §10.6)을 제공하되 기본 활성은 없고 크리티컬 패스에도 없다. |
| Assistants / Threads | Responses API로 대체됨. |
| 관리 UI 내부, 브랜딩, 정적 자산 | 프로토콜이 아니다. |

미구현은 기계가 읽을 수 있는 사유와 함께 `501`. 조용한 `404`는 없다.

## 10. 검증 상태

위 계약들은 추론이 아니라 소스와 스키마에서 읽었다. 두 항목은 **미검증**이며, 가정하는 대신
테스트 스위트에 그렇게 표시한다.

- `n > 1` 스트리밍 시맨틱 — 참조 구현에서 명시적 처리를 찾지 못했다. 다중 choice 스트리밍은
  미명세로 취급하고 과투자하지 않는다.
- 백엔드별 `logprobs` 충실도.
