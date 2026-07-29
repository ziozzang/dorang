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
나머지 80%는 컨트롤 플레인이다. DESIGN §0.3이 추론 프로토콜을 전량 구현하고 관리 표면을 단계적으로
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
| 1.3 | 스트림 도중 오류는 **인밴드**로 전달: `data: {"error":{…}}\n\n` 다음 `data: [DONE]\n\n`. 첫 청크가 이미 나갔으면 HTTP 상태는 이미 200이고 바꿀 수 없다 — DESIGN §7.6이 폴백으로 넘지 않겠다고 한 경계가 정확히 이것이다. |
| 1.4 | 업스트림 스트림이 비면 본문이 비고 `[DONE]`도 **없다**. |
| 1.5 | 업스트림 SSE 원시 줄(`data:`, `event:`, `:`)은 그대로 통과. 구분자가 빠진 프레임만 재프레이밍. |

## 2. chat completions — 청크 JSON 형태

| # | 계약 | 왜 문제가 되는가 |
|---|---|---|
| 2.1 | **없는 필드는 생략되며, 절대 `null`로 나가지 않는다.** | ⚠️ 정정된 근거: 이것은 단순히 Go의 실수를 피하는 문제가 **아니다**. 진짜 OpenAI는 청크에 `"logprobs":null`과 `"finish_reason":null`을 실제로 내보내고, 참조 프록시는 생략한다. 둘이 어긋나며 dorang은 **참조 프록시를 따른다** — 우리 앞의 클라이언트들이 그것을 대상으로 만들어졌기 때문이다. 반대로 고르는 것은 옵션 한 줄이지만, 그것은 사고가 아니라 선택이어야 한다. |
| 2.0 | ⚠️ **필드 매칭은 대소문자를 구분하며, 이것은 스타일이 아니라 보안 속성이다.** | Go의 `encoding/json`은 구조체 태그를 대소문자 무시로 매칭하므로, 구조체 디코드는 `Model`이나 `moDel`로 적힌 키에서 `model` 필드를 채운다. 손으로 쓴 스캐너는 — 그리고 하류의 모든 Python 백엔드는 — 그러지 않는다. 인가 게이트가 스캔하고 어댑터가 언마셜하면, `{"Model":"expensive"}`를 실은 요청은 **모델이 없는 것으로 인가되고 모델이 있는 것으로 디스패치된다**: 허용목록 검사(§7.4)가 그것을 아예 보지 못한다. 따라서 모든 디코드 경로는 대소문자를 구분해야 하고, 게이트와 어댑터가 같은 바이트에서 같은 필드를 해석하는지 차등 테스트로 단언해야 한다. |
| 2.1a | **직렬화기 자체가 규범이다.** | 어느 직렬화기인지 말하지 않는 "바이트 단위" 주장은 무의미하다. 압축 구분자(`:`와 `,` 뒤에 공백 없음), `\uXXXX` 이스케이프가 아닌 원시 UTF-8, 그리고 **Go의 HTML 이스케이프 비활성화** — 켜 두면 Go는 `&&`를 `\u0026\u0026`으로 바꾸며(`<`와 `>`도 각각 `\u003c`, `\u003e`가 된다), 이렇게 하는 서버는 다른 어디에도 없다. `internal/wire/wirejson`과 `internal/wire/anthropic`이 인코더에 `SetEscapeHTML(false)`를 거는 이유다. ⚠️ **호출자가 실제로 제어하는 필드 하나만 빼고는 지켜지고 있었다.** 패키지 인코더에는 `SetEscapeHTML(false)`가 걸려 있어 평범한 구조체 필드는 모두 옳았고, 이를 검사하던 테스트도 모두 그런 필드만 보고 있었다. 그러나 **문자열-또는-배열** 타입은 자기 `MarshalJSON`을 갖는다. 그런 타입 셋 — `openai.Content`, `openai.StopSequences`, `anthropic.BlockList` — 은 기본값이 이스케이프 켜짐인 `encoding/json.Marshal`을 호출했다. 그래서 `a && b <tag>`라고 쓴 사용자 메시지는 옆에 있는 `name`이 그대로 나가는 동안 `a \u0026\u0026 b \u003ctag\u003e`로 상류에 나갔고, 이 항목이 존재하는 이유인 원시 프레임 부분 문자열 검색을 하는 클라이언트는 자기가 보낸 프롬프트를 찾지 못했다. 이 규칙은 바깥쪽 직렬화기 하나가 아니라 본문이 지나가는 **모든** 직렬화기에 적용된다. `TestNoHTMLEscapingOnTheRequestPath`가 문자열 형태, 배열 형태, `stop`을 함께 고정한다. |
| 2.2 | 평범한 텍스트 청크는 정확히 `{id, object, created, model, choices:[{index, delta:{role?,content?}, finish_reason?}]}`. 그 이상 없음. | 키가 더 있으면 그것이 어긋남이다. |
| 2.3 | `object`는 항상 `"chat.completion.chunk"`. | |
| 2.4 | `id`와 `created`는 한 스트림의 **모든 청크에 고정**된다. | 청크마다 `created`를 새로 만들면 그것을 스트림 식별자로 쓰는 클라이언트가 깨진다. |
| 2.5 | `model`은 **모든 청크에서** 클라이언트 대면 이름으로 다시 찍힌다. | DESIGN §7.2를 독립적으로 확증한다: 프레임별 재작성은 피할 최적화가 아니라 요구되는 동작이다. 개정 1의 "첫 프레임 오프셋 패치"는 이 근거만으로도 틀렸다. |
| 2.6 | **각 choice의 첫 델타는 `role: "assistant"`를 싣는다.** | DESIGN §10.7의 스트림 오픈 이벤트이며, Messages 스트림을 변환할 때 `message_start`가 대응되는 자리다. 실제 프로바이더로 재현: 바이트 릴레이는 모든 것을 그대로 흘려보내므로 role을 실어 날랐고, 변환 경로는 떨어뜨렸다 — 한 게이트웨이의 두 클라이언트가, 배포의 업스트림이 어느 패밀리를 말하느냐에 따라 구조가 다른 스트림을 봤다. 별도 프레임이 아니라 첫 델타에 얹으므로 없던 프레임이 생기지 않고, 소스가 이미 role을 말했다면 덮어쓰지 않는다. |
| 2.7 | **`created`는 모든 경로에서 실제 타임스탬프다.** | 스트림 라이터는 찍고 비스트리밍 변환기는 찍지 않아서, chat completion으로 렌더링된 Messages 응답은 `stream:true`가 아닌 한 `"created":0` — 1970 — 을 실었다. 값은 해당 패밀리에 그 멤버가 있으면 업스트림 자신의 것이고, 없으면 게이트웨이의 시계다. `id`를 같은 자리에서 다루는 DESIGN §10.7의 응답 아이덴티티 규칙을 참고. |

## 3. usage

| # | 계약 |
|---|---|
| 3.1 | usage 청크는 `stream_options.include_usage`가 정확히 `true`일 때 **만** 나간다. truthy로는 부족하다. |
| 3.2 | `stream_options`가 없으면 usage는 계산되지만 **와이어에 나가지 않는다**. |
| 3.3 | ⚠️ **OpenAI와의 차이.** 참조 프록시의 usage 청크는 `"choices":[{"index":0,"delta":{}}]`를 담고, OpenAI는 `"choices": []`를 보낸다. dorang은 **참조 프록시를 따른다** — 기존 클라이언트가 그것을 대상으로 만들어졌기 때문이다. `compat.usage_chunk_choices`가 둘 사이를 고르며 **두 값 모두 서비스된다**: `stub`이 기본값이고, `empty`는 엄격한 OpenAI의 `[]`를 낸다. 예전에는 `empty`에서 거부됐다 — 인코더는 작성된 이래 두 형태를 모두 낼 수 있었는데 어떤 설정도 그곳에 닿지 못했다 — 그리고 이제 그 값은 `backend.Call`을 타고 전달된다. `empty`를 고르면 같은 계열 스트림이 바이트 릴레이 우회 경로에서 빠진다 — 그 경로에서 usage 청크는 업스트림의 바이트 그대로이고 dorang이 그 형태를 정하지 않기 때문이다. CONFIG §21a. |
| 3.4 | 일부 백엔드는 `finish_reason` **이후에** usage를 보낸다. 누산기는 finish에서 닫지 말고 늦은 usage를 받아야 한다. |
| 3.5 | usage는 비-OpenAI 확장(비용, 캐시 토큰 상세, 서버 툴 사용)을 실을 수 있다. dorang은 자기 것을 `x-dorang-*` 헤더로 내보내고, 널리 읽히는 usage 확장 필드는 호환을 위해 미러링한다. |

## 4. `finish_reason`

| # | 계약 |
|---|---|
| 4.1 | 백엔드 고유 값은 최소한 다음을 포함하는 매핑 표를 거친다: `end_turn`, `stop_sequence`, `max_tokens`, `tool_use`, `refusal`, `COMPLETE`, `ERROR`, `ERROR_TOXIC`, `eos_token`, `eos`, `STOP`, `SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII`, `IMAGE_SAFETY`, `network_error`, `sensitive`, `guardrail_intervened`. |
| 4.2 | 매핑되지 않은 값은 `"stop"`이 되며 **경고를 남긴다**. 원본 그대로 통과시키지 않는다. |
| 4.2a | ⚠️ **오류 성격의 네이티브 사유도 `"stop"`으로 매핑되며, 그 손실은 명시할 가치가 있다.** `ERROR`, `network_error` 같은 값에는 OpenAI 대응물이 없으므로, 클라이언트는 실패한 턴을 정상 종료로 전달받는다 — 6.4가 필터링된 턴을 정상 턴으로 보고하는 데서 지적하는 것과 같은 해악이다. dorang은 호환을 위해 와이어 매핑을 유지하되 **반드시** 사실을 대역 밖으로 드러낸다: 응답의 `x-dorang-native-stop-reason`, 그리고 원장의 네이티브 값. 구별하고 싶은 호출자에게는 방법이 있고, 그럴 필요가 없는 호출자는 영향을 받지 않는다. |
| 4.3 | 원본 값은 스트리밍·비스트리밍 양쪽 모두에서 **choice 객체** 위 `finish_reason` 옆에 대역 밖으로 보존된다. 이것은 2.2의 "그 이상 없음"에 대한 유일한 문서화된 예외이며, 2.2는 *평범한 텍스트* 청크에 한정된 규칙이다. (한국어판은 오래도록 이 행을 "별도 필드에 보존되므로 실제로 잃는 것은 없다"고 적었다. 4.2a가 그 문장을 뒤집는다 — 와이어의 손실은 실재하고, 보존은 그 손실을 되돌리는 것이 아니라 대역 밖에서 복구 가능하게 만드는 것이다.) |
| 4.4 | ⚠️ **백엔드가 종료 청크를 보내지 않으면 합성한다.** 기본 `"stop"`, **그리고 스트림에서 툴 호출이 관찰됐으면 `"tool_calls"`로 승격**한다. 그냥 전달만 하는 게이트웨이는 툴 호출 턴에 `"stop"`을 내보내 모든 에이전틱 클라이언트를 깨뜨린다. **이 문서에서 가치가 가장 높은 한 줄이다.** |

## 5. 툴 호출 스트리밍

| # | 계약 |
|---|---|
| 5.1 | `delta.tool_calls[].index`는 **필수**이며 optional이 아니다. `id`, `type`, `function`이 optional이다. |
| 5.2 | 인바운드 assistant 메시지는 전달 전에 `tool_calls`에서 `index`가 **제거**되므로, 전체 assistant 메시지를 되돌리는 클라이언트가 거부되지 않는다. |
| 5.3 | 툴 이름은 OpenAI 쪽에서 64자로 제한된다. 절삭은 매핑에 기록하고 **왕복**해야 하며, 아니면 모델의 툴 호출을 되짚을 수 없다. **단순 절삭으로는 부족하다**: 정규화된 툴 이름은 긴 공통 접두사를 공유하는 일이 흔해서 64자에서 자르면 충돌하고 서로 다른 두 툴이 하나가 된다. 축약형은 `접두사 + "_" + 전체 이름 해시의 8자리 16진수`이며, 복원의 정본은 매핑이다. |
| 5.5 | ⚠️ **`max_tokens`와 `max_completion_tokens`는 서로 바꿔 쓸 수 없고, 잘못 고르면 T0 경로가 깨진다.** 널리 배포된 여러 OpenAI 호환 서버는 전자만 받고, 벤더 표면의 현행 리즈닝 모델은 전자를 거부하고 후자를 요구한다. 모든 곳에서 통하는 값이 없으므로 이 필드는 **배포별 설정**이며, 호환 서버 다수파에 맞춰 `max_tokens`가 기본값이다. DESIGN §10.7 참조 — 거기서 이것은 명명된 세 가지 프로토콜 간 함정 중 하나다. |
| 5.4 | 프로토콜 간 tool-use id는 양방향으로 일관되게 정규화해야 한다. |

## 6. `/v1/messages` — 가장 위험한 표면

Anthropic 형태 요청이 OpenAI 형태 백엔드로 가는 일이 매우 흔하므로, 이것은 패스스루가 아니라
어댑터다.

| # | 계약 |
|---|---|
| 6.1 | SSE 프레이밍이 `event: <type>\ndata: <json>\n\n` — chat completions와 달리 **두 줄**이다. |
| 6.2 | 이벤트 타입: `message_start`, `content_block_start`, `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`, 그리고 **`error`**. **`ping` 없음. `[DONE]` 없음.** `message_stop`은 정확히 한 번 — 그리고 `error` 뒤에는 나가지 *않는다*. `error`가 스트림을 그 자체로 종료시키기 때문이다. 이전 초안은 `error`를 빠뜨렸으나, 1.3이 스트림 도중 오류를 인밴드로 요구하고 상태가 200이 된 뒤에는 다른 채널이 없다. |
| 6.3 | 어댑터 경로의 컨텐트 블록 타입: `text`, `tool_use`, `thinking`. |
| 6.4 | ⚠️ `stop_reason`이 세 값으로 붕괴한다: `stop→end_turn`, `length→max_tokens`, `tool_calls→tool_use`, **그 외 전부 → `end_turn`**. 즉 `content_filter`가 정상 종료로 조용히 보고된다. dorang은 호환을 위해 이를 재현하고 **동시에** 실제 사유를 대역 밖으로 알린다 — 다만 **`x-dorang-native-stop-reason`으로 실어 나를 수 있는 것은 비스트리밍 응답뿐이다**. 스트림에서 실제 사유는 마지막 `message_delta` 시점에 알려지는데, 그때는 헤더가 나간 지 한참 뒤다. 그래서 그 값은 원장으로 가고 `x-dorang-request-id`로 조회한다(DESIGN §10.4). 이전 초안은 양쪽 모두에 헤더를 규정했는데, 이 엔드포인트 트래픽의 절반에 대해 구현 불가능한 요구다. |
| 6.5 | 어댑터 경로에서 `stop_sequence`는 `null`. |
| 6.6 | 컨텐트 블록 인덱싱은 **상태를 갖는다**: 보류된 `message_delta`, 청크 큐, 블록 전환 시 합성되는 `content_block_stop` → `content_block_start` 쌍. `content_block_stop`은 항상 마지막 `message_delta`보다 먼저 나가야 한다. 게이트웨이 전체에서 가장 복잡한 상태 기계이며 별도 퍼즈 타깃을 갖는다. |
| 6.7 | 프롬프트 캐싱: 마지막 `message_delta`가 실제 값을 싣고, 캐시 필드는 **0보다 클 때만** 나타난다 — 따라서 `message_start`는 0으로 심는 대신 생략한다. 이전 초안은 "0으로 심는다"와 "0보다 클 때만"을 동시에 적었는데, 둘 다 문자 그대로일 수는 없다. 벤더 자신은 그 자리에 명시적 0을 내보내므로, 벤더에서 딴 골든은 프록시에서 딴 골든과 일치하지 않는다. `input_tokens = prompt − cache_read − cache_creation`(0에서 클램프) — 이 식은 canonical 카운트가 **포함(inclusive)** 일 때에만 타입이 맞는다(DESIGN §10.7). |
| 6.8 | ⚠️ **일부** 참조 프록시 빌드는 비스트리밍 `/v1/messages` 응답에 **비표준 `usage.total_tokens`** 를 넣고 스트리밍 응답에서는 생략하므로, 두 형태가 필드 하나만큼 다르다. `compat.anthropic_total_tokens`가 dorang이 어느 형태를 서비스할지 고르며 **두 값 모두 서비스된다**: `true`가 기본값으로 이 멤버를 추가하고, `false`는 어디에도 `total_tokens`가 없는 엄격한 벤더 형태다. 예전에는 `false`에서 거부됐다. 이 스위치는 **비스트리밍** 쪽만 지배한다 — 스트리밍 메시지는 어느 설정에서도 이 멤버를 담지 않는다. **이것은 보편적이지 않고, 기본값도 보편성에 대한 주장이 아니다:** 2026-07에 측정한 배포는 `{"input_tokens":68,"output_tokens":8}`만 보내고 `total_tokens`는 전혀 내지 않았으므로, *그* 기존 시스템과 바이트 단위로 맞추려면 `anthropic_total_tokens: false`가 필요하다. 기본값이 맞다고 가정하기 전에 기존 시스템을 확인할 것. 이 멤버는 가산적이고 이 패밀리의 모든 SDK는 모델링하지 않은 멤버를 허용하므로, 기본값은 내보내는 쪽으로 기울어 있다. CONFIG §21a. |
| 6.9 | `/v1/messages/count_tokens`는 정확히 `{"input_tokens": <숫자>}` 를 반환하고 `?beta=true`를 받는다. |

## 7. 전 영역 공통

| # | 계약 |
|---|---|
| 7.1 | 오류 봉투: `{"error":{"message":str,"type":str,"param":str\|null,"code":"<문자열>"}}`. **`code`는 숫자가 아니라 문자열이다.** |
| 7.2 | "가용 배포 없음" 조건은 503이 아니라 **429**로. 태그 라우팅 미스는 **401**로 매핑된다. |
| 7.3 | **여섯 가지 인증 헤더 이름**을 받으며 그중 아무거나로 인증된다: `Authorization: Bearer`, `API-Key`, `x-api-key`, `x-goog-api-key`, `Ocp-Apim-Subscription-Key`, 그리고 프록시 고유 헤더. dorang은 여섯 개를 모두 받고 **업스트림으로 전달하기 전에 전부 제거한다.** |
| 7.4 | `GET /v1/models` 항목은 `{"id","object":"model","created":<상수>,"owned_by"}`. `created`는 현재 시각이 아니라 **고정 상수** — 클라이언트가 그것으로 캐싱한다. 목록은 호출 키의 모델 허용목록으로 필터링된다. |
| 7.5 | ⚠️ **라우트 매칭은 구체성 순서다.** `/openai/deployments/{model}/chat/completions`가 `/openai/{endpoint...}`보다 먼저 매칭돼야 한다. 단순 프리픽스 라우터는 구체적 라우트를 catch-all에 조용히 삼킨다. |
| 7.6 | 미지원 파라미터는 기본적으로 **조용히 드롭**되며 거부되지 않는다. 전부 그대로 전달하는 게이트웨이는 클라이언트가 본 적 없는 업스트림 400을 노출시킨다. dorang이 `drop_unsupported: true`를 기본값으로 두는 이유다. **이 규칙은 노브에 관한 것이고, 파라미터가 답(answer)에 관해 무언가를 진술하는 지점에서 멈춘다** — §7.9의 네 행은 드롭이 아니라 구조를 명시한 `400`으로 거부된다. `top_k`를 드롭하는 것은 호출자에게 비용이 없지만, `stop` 시퀀스를 드롭하면 호출자가 배제한 텍스트가 청구된다. |
| 7.7 | 응답 헤더를 읽는 도구가 있다 — 특히 call id와 모델/배포 id. dorang은 `x-dorang-request-id`와 `x-dorang-deployment`를 내보내고, `compat.legacy_headers`가 켜지면 레거시 헤더 이름을 미러링한다. 미러링하는 집합과 dorang이 의도적으로 미러링하지 **않는** 이름은 §7.7a. 이 플래그는 기본 꺼짐이다. |
| 7.8 | 클라이언트가 인바운드 call-id 헤더를 설정하면 그것을 요청 id로 존중한다. |
| 7.9 | **§10.1 구조 표** — 어떤 손실이 거부되고 어떤 손실이 드롭되는지, 그리고 각 항목이 왜 그쪽에 있는지. 버전 관리되며 모든 행이 양방향 변환 테스트를 갖는다. 아래. |

### 7.7a 레거시 헤더 미러 — 이름 하나하나

`compat.legacy_headers: true`는 참조 프록시의 철자를 dorang 자신의 것 **옆에** 덧붙인다.
이름을 바꾸지도, 무엇을 없애지도 않는다. 기본은 꺼짐인데, 이것들이 다른 벤더의 이름이고
그것을 묻지도 않았는데 내보내는 게이트웨이는 자기가 그 벤더라고 주장하는 셈이기 때문이다.

이것이 존재하는 이유는 실패 양상이 조용하기 때문이다. `x-litellm-response-cost`를 읽는
비용 익스포터는 그 헤더가 오지 않기 시작해도 에러를 내지 않는다 — 0을 보고하고, 그 위에
세워진 대시보드도 0을 보고한다. DESIGN §0.3의 "나란히 돌린 뒤 인계받는다"는, 인계가 조용히
숫자를 0으로 만든다면 아무도 수행할 수 없는 마이그레이션이다.

| 레거시 이름 | 미러링하는 dorang 헤더 | 항상 켜지는가? |
|---|---|---|
| `x-litellm-call-id` | `x-dorang-request-id` | 예 |
| `x-dorang-real-model` | `x-dorang-upstream-model` | 예 |
| `x-litellm-model-id` | `x-dorang-deployment` | 예 |
| `x-litellm-response-cost` | `x-dorang-cost-usd`, **그리고 가격이 매겨지지 않았을 때는 `0`** | 스트리밍이 아닌 모든 응답에; **스트림에서는 부재** — 아래 쌍 판별표 참조 |
| `x-litellm-key-spend` | `x-dorang-spend-usd` | detail 전용(§10.4), 그리고 spend를 실제로 조회한 뒤에만 |
| `x-litellm-key-max-budget` | `x-dorang-budget-usd` | detail 전용 |
| `x-litellm-attempted-retries` | `x-dorang-attempt`, **에서 1을 뺀 값** | detail 전용 |
| `x-litellm-response-duration-ms` | `x-dorang-latency-ms` | detail 전용 |

각 미러는 자기가 복사하는 헤더 옆에서 찍히므로 같은 §10.4 detail 게이트를 물려받는다.
원본이 오지 않았는데 미러가 온다면 그것은 "무엇이 항상 켜져 있는가"에 대한 두 번째의,
서로 어긋나는 답이 된다. `attempted-retries`는 의도적으로 복사가 아니다 — dorang은 시도를
1부터 세고 참조 프록시는 재시도를 0부터 세므로, 숫자를 그대로 옮기면 재시도한 적 없는 모든
요청이 재시도 1회로 보고된다.

> 비용 행은 **"예, 항상"** 이라고 적혀 있었고, 세 문단 아래의 쌍 판별표는 같은 헤더가
> 스트림에서 부재한다고 적혀 있었으며, 거기 이름 붙은 테스트는 그 부재를 단언한다. 둘 다
> 참일 수는 없었고, 틀린 쪽이 하필 익스포터가 먼저 읽는 쪽이었다. 이제 열은 판별표가 말하는
> 것을 말한다. `key-spend` 행도 비슷한 이유로 옮겼고 여기서 한 번만 진술한다: spend는 예산
> 홀드에서 읽어 오고, 아무것도 예약하지 않는 요청은 홀드를 하이드레이트하지 않으며, 아무도
> 조회하지 않은 수치는 `0`이 아니라 부재다 — 이 절 전체가 다루는 바로 그 규칙이다.
>
> 한국어판 기록: 위 판별과 "스트림에서는 부재"라는 사실은 §7.7a가 한국어에 없던 동안에도
> 한국어 §7.7 행이 인라인으로 담고 있었고, 그동안 영문 표는 "예, 항상"이라고 적고 있었다.
> 이 문서 쌍에서 한국어가 영문보다 옳았던 지점이며, 지금은 양쪽이 같은 것을 말한다.

**`x-litellm-response-cost`는 그대로 복사가 아닌 유일한 미러이고, 그것은 의도적이다.**
`x-dorang-cost-usd`는 어떤 가격 규칙도 매칭되지 않았을 때 *부재*한다. dorang 자신의 어휘는
"가격 미등록"과 "무료"를 구분하기 때문이다. 레거시 이름에는 그런 구분이 없다 — 그 이름이
속한 프록시는 언제나 그것을 보낸다 — 그래서 사라지는 미러는 이 절이 막으려는 바로 그 실패를,
가격 규칙이 없는 모든 모델에 대해, 조용히 재현하게 된다. 실제로 그것이 출하된 동작이었다:
`pricing:` 블록이 없는 배포는 call id, model id, 재시도 횟수, 소요 시간을 내보내고 비용
헤더는 아예 내보내지 않았다. **쌍으로 읽을 것:**

| `x-litellm-response-cost` | `x-dorang-cost-usd` | 의미 |
|---|---|---|
| `0` | 부재 | 이 모델에 매칭된 가격 규칙이 없다 — 숫자는 0이 아니라 미상이다 |
| `0` | `0` | 가격이 매겨졌고, 이 요청은 아무 비용도 발생시키지 않았다 |
| *n* | *n* | *n*으로 가격이 매겨졌다 |
| 부재 | 부재 | **응답이 스트리밍됐다.** 헤더가 쓰이는 시점에 비용은 결정 불가다. §10.4의 usage 이벤트를 읽거나, `x-dorang-request-id`로 원장을 읽을 것 |

`TestUnpricedRequestStillMirrorsTheCostHeaderAsZero`,
`TestPricedRequestMirrorsTheCostHeaderExactly`,
`TestStreamedRequestDoesNotPublishACostOfZero`,
`TestTheDiscriminatorStillDistinguishesUnpricedFromPriced`가 네 행을 함께 고정하므로,
하나를 고치다가 다른 하나를 조용히 그 안으로 무너뜨릴 수 없다.

**네 번째 행이 앞의 세 행에 대한 정정이다.** 스트림에서는 헤더가 usage보다 앞선다: 응답
헤더는 첫 프레임보다 먼저 나가고 요청은 마지막 프레임 뒤에 정산되므로, 이 헤더가 쓰이는
시점에는 아무것도 비용을 계산하지 않았다. 따라서 미러를 무조건으로 만들면 스트리밍된 모든
요청에 `0`을 게시하게 되는데, 같은 요청의 원장 행에는 실제 수치가 들어 있었다 — 그리고 위 표의
첫 행에 따라 독자는 그 쌍을 "이 모델에는 가격 규칙이 없다"로 해석할 권리가 있다. 비용
익스포터는 스트리밍된 모든 요청에 대해 0을 읽었고, 에이전트 배포에서는 모든 턴이 스트리밍이다.

그 이전 버전은 아무것도 내보내지 않았는데, 그것은 다른 실패이지만 더 작은 실패였다.
**부재는 아무것도 주장하지 않고, 0은 측정을 주장한다.** 그래서 미러는 비용을 알 때와 비용이
없음을 알 수 있을 때 모두 쓰이고, 아직 결정 불가인 단 하나의 경우에만 생략된다. 그 경우에도
숫자는 §10.4가 정확히 이 용도로 이미 갖고 있는 채널에 있다 — 옵트인 `event: dorang.usage`
프레임이 `cost_usd`를 싣고 정산 뒤에 나간다 — 그리고 원장에도 있으며, 응답 헤더로 항상
존재하는 `x-dorang-request-id`로 조인한다.

스트리밍된 모든 턴에 대해 비용이 반드시 있어야 하는 익스포터는 헤더가 아니라 원장을 읽어야
한다. 응답이 아직 모르는 숫자를 실어 나를 수 있는 응답 헤더 배치는 존재하지 않는다.

**dorang이 미러링하지 않는 이름, 그리고 그 이유.** 지어낸 값을 실은 헤더는 부재한 헤더보다
나쁘다. 독자가 그것을 측정과 구별할 수 없기 때문이다.

아래 목록은 예전보다 길어졌고, 그 이유는 적어 둘 가치가 있다: 참조 프록시의 한 실제 배포를
40가지 요청 형태에 대해 측정한 결과 **서로 다른 `x-litellm-*` 이름 17개**가 나왔다. 위에서
모델링한 8개에 대해서다. 모델링되지 않은 것 중 넷이 비용 관련이고 성공 응답 20건 중 17건에
나타난 반면, `x-litellm-response-cost` 자체는 20건 중 1건에 나타났다 — 즉 *그* 배포를 향한
비용 익스포터는 dorang이 모델링하는 이름보다 모델링하지 않는 이름을 읽고 있을 가능성이 더
높다. 이름을 대는 것이 그것을 내보내겠다는 약속은 아니다. 그것은 운영자가 그 간극을 컷오버
중에 발견하느냐, 3주 뒤 정산 대조에서 발견하느냐의 차이다.

| 레거시 이름 | 왜 안 하는가 |
|---|---|
| `x-litellm-version` | dorang은 그 프록시가 아니다. 여기 들어가는 어떤 값도 다른 무언가의 버전이라는 주장이 된다. |
| `x-litellm-model-api-base` | 업스트림의 URL은 배포 토폴로지이지 테넌트가 알 바가 아니다. dorang 헤더에도 올라가지 않는다. |
| `x-litellm-model-region` | dorang에는 배포에 리전 개념이 없다. |
| `x-litellm-attempted-fallbacks`, `x-litellm-max-fallbacks` | dorang은 폴백이 어느 모델*로부터* 왔는지를 기록하지(`x-dorang-fallback-from`), 횟수나 설정된 상한을 기록하지 않는다. 시도 카운터는 재시도와 폴백을 분리하지 않는다. |
| `x-litellm-key-rpm-limit`, `x-litellm-key-tpm-limit` | dorang은 같은 사실을 표준형 `x-ratelimit-limit-requests` / `-tokens`로 게시하며, 그쪽을 이미 읽는 클라이언트가 더 많다. 두 번 미러링하면 서로 어긋날 수 있는 이름이 둘 생긴다. |
| `x-litellm-overhead-duration-ms` | `x-dorang-queue-ms`는 용량 대기이지 프록시 오버헤드가 아니다. 혼동할 만큼 가깝고 같다고 하기에는 멀다. |
| `x-litellm-timeout`, `x-litellm-applied-guardrails` | 요청마다 계산되는 dorang 대응물이 없다. |
| `x-litellm-response-cost-original` | 참조 프록시는 조정 전 비용을 조정 후 비용 옆에 보고한다. dorang의 가격 책정은 조정 규칙을 `Settle` 안에서 적용하고 수치 하나를 게시한다. §8.5의 두 번째 수치는 `x-dorang-notional-usd`이며 그것은 **정가(list-rate)** 대응물이라 다른 양이다 — 할인은 정가가 아니다. 하나를 다른 하나에 미러링하면 그 이름이 뜻하지 않는 숫자를 그 이름 아래 두게 된다. |
| `x-litellm-margin-amount`, `x-litellm-margin-percent`, `x-litellm-discount-amount` | dorang에는 요청 단위의 마진·할인 모델이 없다. 조정 규칙(§8.4)은 더하거나 뺄 수 있지만 마진과 할인으로 분류되지 않으므로, 여기 넣을 요청별 값이 없고 어떤 분해도 지어낸 것이 된다. 이 값들이 필요한 운영자는 `pricing:`에서 유도해야 한다 — dorang이 규칙을 두는 곳이 거기이고, 파생된 숫자를 모든 응답에 되풀이하지는 않는다. |

측정된 배포는 여기 열거하지 않은 이름을 몇 개 더 내보냈다. 관측이 한 배포의 응답에 대한
것이었지 그 프록시의 계약에 대한 것이 아니었기 때문이다: 한 번 본 이름을 적어 두면 dorang이
소유하지도 않은 표면에 대한 약속처럼 읽힌다. **이 절이 하는 주장은 그에 맞춰 한정된다** —
dorang이 무엇을 미러링하며 명시된 각 누락이 왜 누락인지를 말할 뿐, 미러링 집합이 참조 프록시의
어떤 특정 빌드에 대해 완전하다고 말하지 않는다. 위 두 표에 없는 `x-litellm-*` 이름을 읽는
도구를 가진 운영자는 그것이 오지 않을 것으로 예상해야 한다.

### 7.9 §10.1 구조 표 — 무엇이 거부되고 무엇이 드롭되는가

DESIGN §10.1은 변환 손실을 둘로 나눈다. **드롭 가능** 손실은 타깃에 없는 노브다: dorang이
생략하고 `x-dorang-dropped-params`에 열거하며, 요청의 의미는 유지된다. **거부되는** 손실은
`code: unsupported_construct`와 함께 구조를 명시한 `400`이며, 호출자가 그 구조를
`x-dorang-allow-lossy`에 나열한 경우에만 통과한다.

기준은 하나의 질문이고, "타깃이 표현할 수 있는가"가 아니다:

> **없어지면 호출자가 받는 것 또는 청구되는 것이 바뀌는가, 아니면 dorang이 적용한 노브만
> 바뀌는가?**

거부되는 손실은 두 가지 보고 형태를 갖는다. **위치가 있는(located)** 것은 가리킬 인스턴스가
있어 detail이 그것을 나른다(`messages[2].content[1]: application/pdf`). **물질적(material)**
인 것은 요청 파라미터다 — `service_tier`는 하나뿐이고 한 가지를 의미한다 — 그래서 detail이
호출자가 쓴 값을 나르고, `400` 본문의 `param`을 필드 이름으로 채울 수 있다. 위치가 있는
쪽에는 그에 해당하는 것이 없다.

**거부 — 위치가 있는 쪽.** 와이어 파라미터가 없다. 구조는 모양이다.

| 구조 | 없어지면 파괴되는 것 |
|---|---|
| `multi_block_content` | 메시지 content의 배열 형태 |
| `image_block` | 이미지 |
| `document_block` | 문서 전체 |
| `cache_breakpoints` | 캐싱 토폴로지, **따라서 청구서** |
| `thinking_block` | 리즈닝 블록과 거기 붙은 무결성 자료 |
| `multi_block_tool_result` | tool result의 텍스트 아닌 블록 |
| `structured_system` | system 프롬프트의 블록별 속성 |
| `rich_stop_reason` | *어떤* 종료 조건이었는지 |
| `tool_calls` | 툴을 호출하는 능력 자체 |
| `json_schema` | 응답 스키마의 강제 |

**거부 — 물질적인 쪽.** 구조 id가 곧 와이어 파라미터다.

| 구조 | 파라미터 | 없어지면 바뀌는 것 | 요구하지 않는 경우 |
|---|---|---|---|
| `stop` | `stop` / `stop_sequences` | **생성이 언제 끝나는가.** 호출자가 진술한 종료 지점을 넘어 텍스트가 이어지고, 배제한 토큰이 청구되며, 그 시퀀스로 답을 자르는 클라이언트는 동작하지 않는다. 응답 어디에도 종료 지점이 적용되지 않았다는 표시가 없다. | 목록이 비어 있을 때 |
| `n` | `n` | **몇 개의 선택지가 돌아오는가.** `n: 4`에 선택지 하나로 답하면 `choices[3]`은 게이트웨이의 에러가 아니라 클라이언트 안의 인덱스 에러가 된다. | `n: 1` — 기본값을 적어 놓은 것 |
| `logprobs` | `logprobs`, `top_logprobs` | **요청한 응답의 멤버.** 요청한 것이 없는 본문이 "잘 됐다"는 `200`과 함께 돌아온다. | 둘 다 설정되지 않았을 때 |
| `service_tier` | `service_tier` | **가격대.** 호출자가 고르지 않은 가격대에서 실행되고 청구된다 — `cache_breakpoints`가 언제나 거부돼 온 것과 같은 이유다. | `auto`(대소문자 무시): 가격대 선택을 프로바이더에 위임하며, 필드를 생략하는 것과 정확히 같다 |

**드롭하고 이름을 알린다.** 아래 각각은 위 기준에 비추어 검토한 뒤 드롭 가능으로 남긴 것이며,
그 근거가 핵심이다. "답이 바뀐다"는 논거는 너무 많은 것을 증명하고 — 모든 샘플링 노브가 답을
바꾼다 — 모든 요청에 `x-dorang-allow-lossy`를 붙이도록 학습된 호출자에게 그 장치는 아무것도
보고하지 않는다.

| 구조 | 파라미터 | 왜 답이 실질적으로 같은가 |
|---|---|---|
| `logit_bias` | `logit_bias` | **샘플링 사전분포.** 어떤 벤더도 분포를 약속하지 않고 — 같은 요청이 매번 다른 텍스트를 돌려준다 — 따라서 그 부재가 위반할 사후조건이 없으며, 결과는 그 편향을 실은 요청을 다시 뽑은 것과 구분되지 않는다. 여기서 거부하면 같은 논거로 `top_k`와 `frequency_penalty`도 거부해야 한다. |
| `top_k` | `top_k` | 같은 부류이고, 매일 일어나는 경우다. `top_k`는 Messages 네이티브이고 chat-completions에 없으므로, 거부하면 오늘 동작하는 변환을 거부하게 된다. |
| `penalties` | `frequency_penalty`, `presence_penalty` | 같은 부류. |
| `seed` | `seed` | 재현성은 **벤더 문서가 best effort로 명시**한다. 같은 시드가 같은 바이트를 약속하지 않으므로, 드롭된 시드는 보장이 아니라 선호를 제거한다. |
| `parallel_tool_calls` | `parallel_tool_calls` | 표현할 수 없는 타깃은 **병렬 툴 호출을 하지 않는다.** 그곳에서 `false`는 이미 충족돼 있고 `true`는 요구가 아니라 허용이다. 제약이 기본으로 성립한다. |
| `reasoning` | `reasoning` | DESIGN §10.2가 규범적으로 결정한다: 검증되지 않은 리즈닝 능력은 **생략하고 보고**하며, 추측하지 않는다. |
| `priority` | `priority` | DESIGN §10.5는 클라이언트 힌트 무시를 **기본 정책**으로 두며, 이는 능력 격차가 아니다. 거부하면 설정된 동작을 거부하게 된다. |
| `metadata` | `metadata` | 벤더 자신의 대시보드용 라벨. 답에도, 답의 형태에도, 가격에도 들어가지 않는다. |
| `user` | `user` | 동일. |

표가 의도적으로 지키는 두 가지:

- **아무 일도 하지 않는 값은 거부하지 않는다.** `n: 1`과 `service_tier: auto`는 "기본값을
  하라"는 뜻이므로 어떤 능력도 요구하지 않고, 일상적인 호출자에게 아무 비용도 지우지 않는다.
  SDK는 애플리케이션이 요청했든 아니든 `n`을 채운다. 그것에 400을 내는 분류는 대체하려던
  결함보다 나쁜 결함이다.
- **거부는 의도적으로 두 번 일어난다.** 라우팅은 "이 모델의 어떤 배포가 표현할 수 있는가"에
  답하고(DESIGN §7.1), 백엔드는 "선택된 그 배포가 표현할 수 있는가"에 — 그 배포가 선언한 능력
  집합에 대해, DESIGN §4.4의 자체 호스팅 정규화가 끝난 뒤에 — 답한다. 라우팅에서만 막는 것은 두
  능력 집합이 일치하는 동안에만 옳고, 어긋나는 날 조용히 다운그레이드한다.

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
| 벤더 패스스루 catch-all | 감사한 배포에서 그중 어떤 것에도 프로바이더 크리덴셜이 **없었고** 모든 배포에서 패스스루가 비활성이었다. dorang은 설정으로 켤 수 있도록 범용 엔진(DESIGN §10.6)을 제공하되 기본 활성은 없고 크리티컬 패스에도 없다. |
| Assistants / Threads | Responses API로 대체됨. |
| 관리 UI 내부, 브랜딩, 정적 자산 | 프로토콜이 아니다. |

미구현은 기계가 읽을 수 있는 사유와 함께 `501`. 조용한 `404`는 없다.

## 10. 검증 상태

위 계약들은 추론이 아니라 소스와 스키마에서 읽었다. 두 항목은 **미검증**이며, 가정하는 대신
테스트 스위트에 그렇게 표시한다.

- `n > 1` 스트리밍 시맨틱 — 참조 구현에서 명시적 처리를 찾지 못했다. 다중 choice 스트리밍은
  미명세로 취급하고 과투자하지 않는다.
- 백엔드별 `logprobs` 충실도.

---

## 11. 오류 분류 체계 — 우리 어휘가 아니라 벤더의 어휘

오류는 API 표면이다. 클라이언트는 그 위에서 분기한다: SDK는 상태와 `type`으로 재시도 여부를
정하고, 애플리케이션 코드는 흔히 `code`를 매칭한다. 자기만의 어휘를 지어내는 게이트웨이는 필드
이름을 바꾸는 게이트웨이만큼 비호환이다 — 다만 첫 요청이 아니라 누군가의 재시도 루프 안에서,
더 늦게 실패할 뿐이다.

그래서 dorang은 호출자가 어느 패밀리를 말하는지에 따라 **벤더 자신의 오류 어휘**를 내보내고,
모든 업스트림 형태를 그리로 정규화한다. 백엔드들이 충분히 제각각이라 이 작업은 피할 수 없다:
어떤 엔진은 정수 `code`와 Python 예외 이름을 `type`으로 보내고, 한 서버가 하나의 조건에 대해
네 개의 호출 경로에서 네 가지 `type` 철자를 만들어 내는 것이 관측됐다.

### 11.1 봉투

**OpenAI 패밀리** — `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`,
`/v1/responses`, 그리고 나머지:

```json
{"error":{"message":"…","type":"invalid_request_error","param":"model","code":"model_not_found"}}
```

`code`는 **문자열 또는 null**이며 결코 숫자가 아니다. `param`은 문제가 된 필드이거나 null이다.

**Anthropic 패밀리** — `/v1/messages`, `/v1/messages/count_tokens`:

```json
{"type":"error","error":{"type":"invalid_request_error","message":"…","param":"model","code":"model_not_found"}}
```

바깥의 `"type":"error"`가 핵심이다: Anthropic SDK가 그것으로 디스패치한다. OpenAI 객체만 실은
본문은 그런 클라이언트에게 철자만 다른 오류가 아니라, **아예 분류할 수 없는** 오류다. 그
클라이언트가 스위치하는 멤버가 없기 때문이다. 봉투가 패밀리의 함수가 되기 전까지 dorang이 이
패밀리에서 돌려준 모든 오류가 그 본문이었다: §11.2의 `type` 열을 사영한 결과가 다른 패밀리의
객체 안에 담겼고, 그곳에서는 아무도 그것을 읽지 않는다.

⚠️ **`param`과 `code`도 여기 함께 나가며, 이것은 실수가 아니라 의도된 합집합이다.** 벤더 자신의
객체에는 둘 다 없고 §7.1은 둘 다 요구하므로, 둘 중 하나만 내보내면 어느 한쪽 독자가 깨진다:
바깥 type을 빼면 이 패밀리의 SDK가 실패를 분류하지 못하고, `param`/`code`를 빼면 §7.1을 보고
작성된 클라이언트가 없는 키를 읽는다. 이 패밀리의 모든 SDK는 오류 객체의 모르는 멤버를
허용하므로, 합집합은 양쪽을 다 만족시키고 어느 쪽도 거부해야 할 필드를 보지 않는다. 이는 또한
§11.2의 `code` 열 — 애플리케이션 코드가 매칭하는 대상이자 그 표의 정보 대부분 — 이 아무것도
분기할 수 없는 산문 속으로 접히는 것을 막는다. 이 절의 이전 개정판은 여기에 `code`도 `param`도
없다고 적었는데, 그런 형태를 내보낸 빌드는 한 번도 없었다.

봉투는 **호출자**가 말하는 패밀리로 고르며, 모든 오류 응답이 지나는 단 하나의 경로에서 고른다:
`(*server.Error).ForFamily`가 사영된 type과 함께 패밀리를 기록하고, `server.appendEnvelope`가
그 패밀리의 객체를 렌더링한다. Anthropic 쪽은 그 직렬화기를 이미 소유하고 골든 테스트하는
`internal/wire/anthropic`에 위임하며, 하나의 계약에 두 번째 렌더러를 두지 않는다. 하나의 봉투에
렌더러가 둘이라는 것이 바로 이 결함을 만들어 낸 원인이다. 둘 중 어느 패밀리도 아닌 모든 것 —
models, health, metrics, admin, passthrough, 그리고 어떤 라우트에도 매칭되지 않은 요청이 갖는
제로 값 — 은 `server.Family.Anthropic`의 화이트리스트에 의해, 누락이 아니라 의도적으로 OpenAI
객체로 답한다. `TestCompat11EnvelopeIsDispatchableOnBothFamilies`가 §11.2의 모든 행을 두 패밀리로
통과시키고 바이트에서 바깥 판별자를 디코드한다.

#### 11.1a 스트림 도중 오류 프레임

첫 프레임이 나간 뒤에는 상태가 200이고 바꿀 수 없으므로(§1.3) 오류는 인밴드로 간다 — 그리고 두
패밀리는 그것을 같은 방식으로 프레이밍하지 않는다. 위의 봉투는 답의 절반일 뿐이고 프레이밍이
나머지 절반이며, 프레이밍을 틀리는 것이 봉투를 틀리는 것보다 나쁘다. 클라이언트가 그 프레임을
아예 보지 못하기 때문이다.

| 패밀리 | 프레임 |
|---|---|
| OpenAI | `data: {"error":{…}}\n\n` 다음 `data: [DONE]\n\n` (§1.1, §1.2) |
| Anthropic | `event: error\ndata: {"type":"error","error":{…}}\n\n`, 그리고 **그 뒤에는 아무것도 없음** (§6.1, §6.2) |

Anthropic 스트림을 읽는 클라이언트는 이벤트 **이름**으로 디스패치한다. 따라서 data만 있는
프레임은 잘못 파싱되는 프레임이 아니라 조용히 버려지는 프레임이고, 그 결과 실패한 교환과 잘린
교환을 구별할 수 없게 된다. 반대 방향에서 Anthropic 스트림 안의 `[DONE]`은 규격을 지키는 파서가
이름 붙일 수 없는 프레임이며, `message_stop`은 `error` 뒤에 와서는 안 된다: 메시지는 멈춘 것이
아니라 실패한 것이다. `TestMidStreamErrorUsesTheFamilysFraming`이 둘 다 고정하며, 부분 문자열
매칭이 아니라 프레임을 파싱해서 확인한다.

### 11.2 canonical 조건

한 행이 하나의 내부 조건이다. dorang은 이 표 밖의 `type`을 결코 내보내지 않는다 — 두 type 열에
있는 여덟 개 문자열이 어휘의 전부이며, `TestEveryTypeOnTheWireIsInTheTable`이 모든 상태를 두
패밀리로 통과시키고 아홉 번째가 나오면 실패한다. `timeout_error`, `not_implemented_error`,
`service_unavailable_error`는 `internal/server`에 선언돼 있고 **어느 벤더의 어휘에도** 없다.
앞의 둘은 실제 504와 501에서 나가고 있었는데, 이 표는 그 자리에서 두 열 모두 `api_error`라고
말한다. 이제 접혔다. 잃는 것은 없다: 501에서 클라이언트가 행동의 근거로 삼는 것은 code —
`route_not_implemented`냐 `route_unknown`이냐(DESIGN §0.2) — 이고 나머지는 상태가 나른다.

두 `type` 열은 **상태의 같은 함수가 아니다**. 404는 OpenAI 클라이언트에게
`invalid_request_error`이고 Anthropic 클라이언트에게 `not_found_error`다. 413은
`invalid_request_error`와 `request_too_large`다. 이 사영은 한 곳에 산다 —
`server.TypeForFamily`, 그리고 모든 오류 응답이 지나는 단 하나의 경로에서
`(*server.Error).ForFamily`가 그것을 적용한다 — 그래서 새 조건이 어느 패밀리의 철자를 실수로
고를 수 없다. 두 개의 429 용량 행만이 상태가 아니라 *조건*으로 type이 결정되며, 그 둘은
raise되는 자리에서 대체 철자를 명시적으로 지정한다.

| 조건 | HTTP | OpenAI `type` | OpenAI `code` | Anthropic `type` |
|---|---:|---|---|---|
| 잘못된 요청 본문 | 400 | `invalid_request_error` | `invalid_request` | `invalid_request_error` |
| 알 수 없거나 허용되지 않은 파라미터 | 400 | `invalid_request_error` | `invalid_parameter` | `invalid_request_error` |
| 구조적 다운그레이드 거부(§10.1) | 400 | `invalid_request_error` | `unsupported_construct` | `invalid_request_error` |
| 컨텍스트 윈도 초과 | 400 | `invalid_request_error` | `context_length_exceeded` | `invalid_request_error` |
| 예산 소진(종단, §6.4) | 400 | `invalid_request_error` | `budget_exceeded` | `invalid_request_error` |
| 크리덴셜 누락 또는 형식 오류 | 401 | `authentication_error` | `invalid_api_key` | `authentication_error` |
| 만료되거나 폐기된 크리덴셜 | 401 | `authentication_error` | `invalid_api_key` | `authentication_error` |
| 키의 허용목록에 없는 모델 | 403 | `permission_error` | `model_not_allowed` | `permission_error` |
| 이 키에 허용되지 않은 라우트 | 403 | `permission_error` | `route_not_allowed` | `permission_error` |
| 차단된 키 | 403 | `permission_error` | `key_blocked` | `permission_error` |
| 알 수 없는 모델 | 404 | `invalid_request_error` | `model_not_found` | `not_found_error` |
| 알 수 없는 리소스(file, batch, response) | 404 | `invalid_request_error` | `not_found` | `not_found_error` |
| 크기 상한을 넘은 본문 | 413 | `invalid_request_error` | `request_too_large` | `request_too_large` |
| 레이트 리밋(RPM/TPM) | 429 | `rate_limit_error` | `rate_limit_exceeded` | `rate_limit_error` |
| 프로바이더 쿼터 소진(§6) | 429 | `rate_limit_error` | `insufficient_quota` | `rate_limit_error` |
| 가용 배포 없음 | 429 | `rate_limit_error` | `no_healthy_deployment` | `overloaded_error` |
| 용량 대기 타임아웃(§5.4) | 429 | `rate_limit_error` | `capacity_unavailable` | `overloaded_error` |
| 게이트웨이 결함 | 500 | `api_error` | `internal_error` | `api_error` |
| 폴백 이후의 업스트림 5xx | **업스트림 자신의 5xx**, 그대로 통과 | `api_error` | 업스트림이 문자열 `code`를 보냈으면 그 문자열, 아니면 상태를 문자열로 | `api_error` |
| 업스트림이 dorang이 쓸 수 있는 것을 아무것도 답하지 않음 | 502 | `api_error` | `upstream_*`, 어느 방향으로 실패했는지를 지칭 | `api_error` |
| 선언됐으나 미구현인 라우트(§0.2) | 501 | `api_error` | `route_not_implemented` | `api_error` |
| 업스트림 타임아웃 | 504 | `api_error` | dorang 자신의 데드라인이 터졌으면 `timeout`, 아니면 업스트림 5xx 행과 동일 | `api_error` |

이 중 셋은 그럴듯한 대안이 틀렸기 때문에 근거를 적어 둘 가치가 있다:

- **예산 소진은 429가 아니라 400이다.** `429`는 재시도를 부르고 그 조건을 폴백 트리거로 표시하는데
  (§7.6), 그러면 호출자가 요청한 적 없는 모델에 *다른* 주체의 예산을 쓰게 된다. 예산은 종단이며,
  상태가 그렇게 말해야 한다.
- **허용목록에 없는 모델은 401이 아니라 403이다.** 크리덴셜은 정상적으로 인증됐고, 다만 이 모델이
  허용되지 않았을 뿐이다. `401`은 클라이언트에게 재인증하라고 말하는데 그것은 도움이 되지 않는다.
  한 참조 프록시는 여기서 `401`을 답한다 — dorang은 따르지 않는다. 그 안내가 클라이언트를 적극적으로
  오도하기 때문이다.
- **가용 배포 없음은 503이 아니라 429다.** `Retry-After`를 동반한 용량 조건이고, 클라이언트는 이미
  `429`에서 올바르게 백오프한다.

- **업스트림 5xx는 자기 상태를 유지하며, 502로 접히지 않는다.** 프로바이더의 `500`은 dorang을
  떠날 때도 `500`이고 `502`는 `502`다. 이 행은 감사가 실측하기 전까지 "폴백 이후의 업스트림 5xx →
  502"라고 적혀 있었다: dorang은 언제나 상태를 그대로 통과시켜 왔고(`backend.upstreamError`가
  업스트림의 상태를 `server.Normalize`에 그대로 넘긴다), 이 통과가 모든 업스트림 실패에서 dorang을
  기존 시스템과 상태 동일하게 유지해 준다 — 이것을 찾아낸 재현에서 그런 실패가 아홉 건이었다.
  접는 쪽은 사실이 아닐 뿐 아니라 더 나쁘다: `502`는 *dorang이* 동작하는 백엔드에 닿지 못했다는
  뜻이고, `502`에서 재시도하고 `500`에서 포기하는 클라이언트는 프로바이더가 이미 퇴역시킨 모델을
  재시도하라는 말을 듣게 된다. `502`는 dorang이 쓸 수 있는 답을 전혀 받지 못한 경우 — 응답 없음,
  읽을 수 없는 본문, 거부된 리다이렉트, 이미 시작된 스트림 내부의 실패 — 를 위해 남겨 둔다. 그것은
  프로바이더가 요청을 어떻게 판단했는가가 아니라 *홉*에 관한 진술이다.
  `TestUpstream5xxKeepsItsOwnStatus`가 이를 고정한다.

업스트림 5xx 행과 504 행의 `code` 열이 그렇게 적힌 이유는 §11.3의 보존 규칙이 먼저이기 때문이다:
쓸 수 있는 **문자열** code를 보낸 백엔드는 이미 §7.1을 만족하며, 그 code는 `upstream_error`보다
클라이언트에게 더 유용하다. 그것은 그대로 통과한다. 봉투에 들어갈 수 없는 code — 숫자, 객체,
배열 — 는 dorang의 canonical code로 대체되고 `x-dorang-native-error-code`에 보존된다. 이 표의
이전 개정판은 무조건 `upstream_error`를 규정했는데, 그런 형태를 내보낸 빌드는 한 번도 없었고 두
절 뒤의 §11.3과도 모순된다.

#### 11.2a dorang 자신의 거부

아래 조건들은 참조 어휘에 대응 조건이 없어서 위 표에 행이 없다. 다음 감사가 이것들을 드리프트로
읽지 않도록, 그리고 각각이 *왜* 따로 있는지가 다시 유도되는 대신 적혀 있도록 열거한다.

각각은 **고치는 방법이 다른** 거부다. 여기 속하는지의 기준은 그것뿐이다: 호출자가 다르게 행동할
수 없는 조건은 §11.2의 code를 받고 위 행들로 접힌다. `type` 열은 §11.2가 쓰는 것과 같은 사영이므로,
여기 있는 어떤 것도 type 어휘를 벗어나지 않는다.

이것은 전체 code 레지스트리가 아니다 — dorang은 어떤 클라이언트도 분기하지 않는 조건에 대해
운영용 code도 내보낸다(`method_not_allowed`, `admin_required`, `catalog_not_configured`,
`passthrough_*` 계열). 그것들은 철자가 안정적인 진단 텍스트이고, 이 표는 클라이언트가 *매칭할*
것으로 기대되는 code를 위한 것이다.

| 조건 | HTTP | OpenAI `type` | OpenAI `code` | Anthropic `type` |
|---|---:|---|---|---|
| 회전으로 퇴역한 시크릿(§11.2c) | 401 | `authentication_error` | `secret_retired` | `authentication_error` |
| 저장된 크리덴셜이 미지원 해시 스킴을 씀 | 401 | `authentication_error` | `unsupported_hash_scheme` | `authentication_error` |
| 레거시 해시 스킴 비활성 | 401 | `authentication_error` | `legacy_scheme_disabled` | `authentication_error` |
| 레거시 임포트 윈도 종료 | 401 | `authentication_error` | `legacy_window_closed` | `authentication_error` |
| 토큰 가드가 보류시킨 키(§11.6) | 403 | `permission_error` | `credential_pended` | `permission_error` |
| 소유 사용자나 팀이 없는 키 | 403 | `permission_error` | `no_principal` | `permission_error` |
| 크리덴셜 저장소에 도달 불가 | 503 | `api_error` | `auth_unavailable` | `overloaded_error` |
| 알 수 없는 라우트(단지 미구축이 아니라) | 501 | `api_error` | `route_unknown` | `api_error` |
| 구조 핀을 라우팅할 수 없음(§B.2) | 503 | `api_error` | `state_pin_unroutable` | `overloaded_error` |
| 크리덴셜 핀을 라우팅할 수 없음 | 503 | `api_error` | `credential_pin_unroutable` | `overloaded_error` |
| 크리덴셜 핀 소진 / 포화 | 503 | `api_error` | `credential_pin_exhausted`, `credential_pin_saturated` | `overloaded_error` |
| 모든 폴백 후보를 이미 시도함(§7.6) | 503 | `api_error` | `fallback_exhausted` | `overloaded_error` |
| 폴백 홉 또는 벽시계 예산 소진 | 가변 | 상태에 따라 | `max_hops_exhausted`, `fallback_budget_elapsed` | 상태에 따라 |
| 스트림이 이미 커밋되어 홉 불가(§7.6) | 503 | `api_error` | `stream_committed` | `overloaded_error` |

`secret_retired`가 논쟁할 가치가 있는 항목이다. `invalid_api_key`가 거의 맞기 때문이다. 퇴역한
시크릿의 해법은 "마지막 회전이 발급한 시크릿을 쓰라"이지 "새 키를 받으라"가 아니다. 그것을
`invalid_api_key`로 보고하면 호출자를 재발급으로 보내는데, 그것이 바로 회전이 피하려고 존재하는
일이다. 같은 논리가 `credential_pended`를 `key_blocked`와 떼어 놓는다: 하나는 운영자가 한 번의
조작으로 풀 수 있는 통계적 판단이고 다른 하나는 운영자가 내린 결정이며, 둘을 구별하지 못하는 지원
티켓은 엉뚱한 화면으로 간다.

`invalid_api_key` *안으로* 접힌 것들: `missing_credential`, `malformed_credential`,
`invalid_credential`(두 번 — 알 수 없는 키와 다이제스트 불일치), `credential_expired`. 철자 다섯 개,
클라이언트가 볼 수 있는 조건 하나, 그리고 클라이언트의 `invalid_api_key` 분기가 빗나갈 방법 다섯 개.
구별은 message에 남아 있고, §11.1이 이미 말했듯 Anthropic 패밀리에서 `code`가 실어 나를 것을 접어
넣는 자리가 거기다.

### 11.3 네이티브 오류는 보존하되 결코 전달하지 않는다

업스트림 자신의 `type`과 `code`는 `x-dorang-native-error-type` / `x-dorang-native-error-code`로
드러난다. 응답 본문에는 **넣지 않는다**. 그것을 전달하면 이 절이 고치려고 존재하는 바로 그 결함을
재현하게 된다 — `type`으로 분기하는 클라이언트가 벤더 고유 문자열에서 잘못 분기한다 — 그리고 이것은
§4.2a가 stop reason에 쓰는 것과 같은 대역 밖 패턴이다.

`x-dorang-native-error-code`는 백엔드 자신의 code가 봉투의 code가 *될 수 없었을* 때에만 그것을
나른다: §7.1이 문자열을 요구하는데 숫자였거나, 스칼라를 요구하는데 JSON 객체나 배열이었을 때다.
평범한 문자열 code를 보낸 백엔드는 이 헤더를 세우지 않는다. 그 code는 이미 봉투 안에 있고, 모든
오류에서 그것을 되풀이하는 헤더는 아무것도 알리지 못하기 때문이다. 두 헤더 모두 128바이트로
클램프되고 제어 문자가 있으면 거부된다. 백엔드는 호출자보다 덜 신뢰되는 출처이고, 호출자 경로는
이미 검사되고 있다.

#### 11.3a 업스트림의 *메시지* 는 클라이언트가 아니라 운영자에게 간다

`x-dorang-native-error-message`는 없고, 앞으로도 의도적으로 없을 것이다.

메시지를 본문 밖에 두는 이유는 단정함이 아니라 구체적이다: 여러 OpenAI 호환 서버가 401에 문제의 키를
메시지에 인용해서 답한다. 그래서 업스트림의 텍스트를 자기 봉투에 복사하는 게이트웨이는, 어떤
크리덴셜이 무효이거나 회전 중일 때 마침 호출하고 있던 테넌트에게 운영자의 프로바이더 크리덴셜을
넘겨준다. 적대적 백엔드는 그런 상황을 기다릴 필요도 없다 — 방금 받은 `x-api-key` 헤더로 아무 요청에나
답하면 된다.

**이 논리는 응답 헤더에도 그대로 옮겨 간다.** 헤더는 같은 클라이언트가, 같은 연결로, 지금까지 쓰인
모든 HTTP 라이브러리로 읽는다. 텍스트를 본문에서 헤더로 옮기는 것은 유출을 막는 것이 아니라 옮기는
것이다. `type`과 `code`는 대역 밖으로 나가는데, 그것들이 자유 텍스트가 아니라 열거된 토큰이기
때문이다.

그래서 메시지는 **운영자 로그**로 가고, `x-dorang-request-id`로 요청에 조인되며, 그 요청이 실제로
실었던 시크릿으로 `internal/redact`가 스크러빙한다. 그것은 데이터베이스 왕복 없이 복구 가능하고,
장애를 디버깅하는 운영자에게 필요한 것이 그것이다. 클라이언트가 받는 것은 그 상태에 대한 dorang
자신의 문구, canonical `code`, 그리고 — 백엔드가 쓸 만한 것을 보냈을 때 — 봉투 안의 백엔드 자신의
문자열 `code`다. 그것만으로 답이 되는 경우도 많다: `model_retired` 같은 code는 문장 없이도 조건을
지목한다.

이것은 기존 시스템 대비 **알려진 충실도 저하**다. 기존 시스템은 업스트림의 문장을 본문에 넣는다.
이것은 의도적이다. 클라이언트에게 그것을 복원하는 것은 버그 수정이 아니라 보안 검토가 따라붙는 새
결정이다.

### 11.4 `Retry-After`는 모든 429와 503에 필수다

detail 헤더(§10.4) 뒤에 가려지지 않는다. 클라이언트가 그것을 보고 행동하며, 그것이 없으면 모든 SDK의
백오프가 고정된 추측으로 퇴화한다. 업스트림이 하나를 주면 그것을 존중하고, 주지 않으면서 조건이
dorang 쪽 대기라면 dorang이 쿼터 리셋 시각이나 용량 큐에서 자기 추정치를 만들어 준다.

> ⚠️ **한 가지 경우는 다루지 못하며, 발견되도록 두는 대신 진술해 둔다.** "업스트림이 하나를 준다"는
> 것은 문자 그대로의 `Retry-After` 헤더를 뜻한다: `internal/backend`의 파서는 그 이름만 읽고 다른
> 이름은 읽지 않는다. `x-ratelimit-reset-requests`만 실은 `429`로 답하는 업스트림은 — 지연이 아니라
> 윈도 리셋 시각을 준 것이므로 — 이 게이트웨이가 전달하는 것을 아무것도 주지 않는다.
> `router.Outcome.ResetAt`이 그것을 나를 필드인데 **트리 어디에도 생산자가 없어서**, 리셋은 이 헤더에도
> §7.6의 cooldown에도 닿지 않고, 프로바이더가 지목한 윈도 동안 해당 배포가 선택에서 빠지지 않는다.
> dorang *자신의* 쿼터 소스는 이 헤더를 올바르게 채운다. 영향을 받는 것은 업스트림이 신호한 리셋뿐이다.
> DESIGN §17.1 하네스 표의 세 번째 행으로 추적 중이며, 발견된 곳도 거기다: 시나리오 하네스는 리셋
> 헤더를 읽었고 프로덕션은 한 번도 읽지 않았다.

> ⚠️ **두 번째로 다루지 못하는 경우 — 503.** 이 절의 제목은 429와 503 둘 다를 말하지만, 현재 빌드에서
> `Retry-After`를 응답에 붙이는 코드 경로는 두 곳뿐이고 둘 다 **429에서만** 붙인다
> (`server.stampHeaders`, `server.WriteError`; 둘 다 `status == http.StatusTooManyRequests`로 게이트한다).
> `internal/backend`는 업스트림이 503에 `Retry-After`를 보내면 그 값을 `Error.RetryAfterSeconds`로
> 읽어 들이지만, 응답 경로가 그것을 다시 버린다. 따라서 §11.2a의 여섯 개 503 조건 —
> `auth_unavailable`, `state_pin_unroutable`, `credential_pin_unroutable`, `credential_pin_exhausted`,
> `fallback_exhausted`, `stream_committed` — 은 오늘 `Retry-After` 없이 나간다. 위 문단이 진술하는
> 규칙은 그대로 규범이다. 다만 503 절반은 아직 구현되지 않았고, 영문 §11.4는 이 사실을 아직 적지
> 않았다. 이 문단은 코드에서 확인한 것이지 계약의 완화가 아니다.
