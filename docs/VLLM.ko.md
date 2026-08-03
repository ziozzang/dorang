# day-zero 백엔드로서의 vLLM

> `vllm-project/vllm` 커밋 `7aea73d`(main, 2026-07-28)의 소스에서 읽어낸 통합 명세.
> 아래 모든 동작 주장은 추론이 아니라 그 트리에서 직접 확인한 것이다. 릴리스가 아니라 커밋을
> 인용하는 이유는 체크아웃이 shallow이고 태그가 없어 버전 번호를 실제로 알 수 없기 때문이다.
>
> English (기준 문서): [VLLM.md](VLLM.md)

vLLM은 "OpenAI 호환 백엔드 + priority 필드 하나"가 아니다. **조용히** 어긋난다 — 받아들이고,
무시하고, 동작하는 것과 구별되지 않는다. 이 문서가 존재하는 이유는 통합 리스크의 대부분이
`200`을 반환하는 것들에 있기 때문이다.

---

## 1. dorang 설계를 바꾼 세 가지 발견

### 1.1 미선언 컨텍스트 윈도우는 인증 없는 GET 한 번으로 해결된다

```
GET {base_url}/v1/models  →  data[i].max_model_len
```

이 필드는 모델 카드에 붙은 vLLM 확장이며 **실효(override 이후) 값**을 담는다 — 128K 모델에
`--max-model-len 8192`를 주면 `8192`를 보고하고, 메모리 기반 auto-fit 결과도 서빙 전에 다시
동기화된다. 플래그가 필요 없고, 라우트는 무조건 등록된다.

이것으로 설계 §4.3이 말하는 간극이 모든 vLLM 배포에 대해 닫힌다. dorang은 컨텍스트 윈도우를
추측할지 미선언으로 둘지 고르지 않아도 되고, 사실을 읽는다.

가정이 아니라 코드로 박아야 할 두 규칙:

- **`null`은 0이 아니라 미선언이다.** LoRA 어댑터 카드는 이 필드를 아예 생략하고, 기반 모델을
  가리키는 `parent`를 갖는다. 윈도우는 그쪽에서 상속한다.
- **읽을 수 있는 max-output 값은 없고, dorang이 합성해서도 안 된다.** vLLM에 그런 개념 자체가
  없다 — 요청마다 `max_model_len − input_length`를 계산한다. 미선언으로 둘 것. 예외는
  `/v1/completions`로, 여기서 `max_tokens`는 **16**이 기본이고 명시적 `null`도 16으로 강제되므로
  dorang이 실제 값을 보내야 한다.

`POST /tokenize`도 `max_model_len`을 필수 필드로 반환해 교차 검증에 쓸 수 있지만, 토큰화 패스
비용이 들고 기동 시점에는 불필요하다.

### 1.2 `priority`는 기본값에서 조용히 무시되며, 필드 설명 자체가 거짓이다

모든 `priority` 필드의 설명은 0이 아닌 값이 "서빙 모델이 priority 스케줄링을 쓰지 않으면 에러를
발생시킨다"고 말한다. **그런 검증은 존재하지 않는다.** 그 강제는 이미 삭제된 엔진 세대에 있었다.

기본 스케줄링 정책은 `fcfs`이고, 그 아래에서 값은 엔진의 요청 객체까지 그대로 흘러가 소비자가
없다. 기본 설정 서버에 `priority: 5`를 보낸 게이트웨이는 **200 OK, 경고 없음, 효과 없음**을 얻는다.

> dorang은 이것을 응답으로 탐지할 수 없다. 정책을 노출하는 유일한 HTTP 표면은 프로덕션에서 절대
> 켜면 안 되는 개발 모드 엔드포인트다. 그래서 정직한 답은 **오퍼레이터 선언**이다: vLLM 프로바이더
> 설정이 `--scheduling-policy priority`를 켰는지를 기록하고, 그것 없이 priority를 선언한 배포는
> **미검증 능력(unverified capability)** 으로 드러낸다 — §4.3이 리즈닝에 쓰는 것과 같은 패턴이다.

**방향 확인: 낮은 값이 먼저 스케줄된다.** 소스에서 세 가지 독립 확증 — 큐 docstring, min-heap에
들어가는 비교 연산자, 설정 문서. dorang의 기존 클래스 맵(`realtime: 0, interactive: 2, batch: 10`)은
이미 옳고 바꿀 필요가 없다.

추가 제약 세 가지:

- **`service_tier`는 받아들여지고 소비자가 없다.** 이 백엔드에서 priority 대체 수단으로 절대 쓰면
  안 된다.
- **`/v1/messages`에는 `priority` 필드가 아예 없고**, 요청 모델이 알 수 없는 키를 조용히 버린다.
  priority가 필요한 Anthropic 형태 트래픽은 `/v1/chat/completions`로 번역해야 한다.
- **`/v1/responses`는 priority를 변형한다**: 내장 툴 왕복마다 1씩 감소시키므로 툴 루프의 2턴부터는
  요청보다 한 단계 더 긴급하게 돈다. 그 엔드포인트를 툴과 함께 쓴다면 priority 밴드를 **2 이상**
  띄워야 한다.

priority는 **선점(preemption)을 의미하지 않는다.** 대기 요청이 실행 중 요청을 밀어낼 수 없고,
큐의 앞으로 가서 블록이 비기를 기다린다. priority는 admission 순서와 eviction 선택을 지배하며,
실행 중 요청들 사이의 스텝별 할당은 지배하지 않는다.

### 1.3 batch job API는 없으며, 이전의 모호함은 이제 정리됐다

`/v1/batches`는 **존재하지 않는다** — 소스 트리 어디에도 그 문자열이 없다.

`/v1/chat/completions/batch`는 실제로 등록된 라우트로 **존재한다**. 그래서 예전 주장은 절반은
맞았다. 하지만 그것은 **동기식, 단일 요청, N개 대화 팬아웃**이다: `messages`가 *대화들*의 리스트이고,
각각이 독립 엔진 요청이 되며, 결과는 `choices`가 `0..N−1`로 인덱싱된 평범한 응답 하나다. job id 없음,
폴링 없음, 취소 없음, 영속성 없음. `stream`, `tools`, `n > 1`, beam search를 거부한다.

`run_batch`는 자체 엔진을 만들고 라우트를 하나도 등록하지 않는 **오프라인 CLI**다. 서빙할 수 있는
HTTP는 Prometheus 스크레이프 포트뿐이다. 백엔드로 도달 불가능하다.

> **batch 스케줄러가 dorang 자신의 것이라는 §11.1의 결정은 옳음이 확인됐고 재론할 필요가 없다.**
> 팬아웃 라우트를 §11.1이 허용하는 선택적 가속기로 나중에 채택한다면, 그것이 N개 행을 하나의
> all-or-nothing HTTP 요청으로 접는다는 점에 유의할 것 — §11.1의 "부분 실패는 예상된다"와 충돌하며,
> 그것이 이것이 기반이 아니라 가속기인 이유다.

---

## 2. 게이트웨이 코드를 바꾸는 조용한 divergence

**얼마나 조용히 실패하는지** 순으로 정렬.

| # | Divergence | 결과 |
|---|---|---|
| 2.1 | **`tools`와 함께 `tool_choice: null`을 보내면 검증을 통과한 뒤 툴 파싱이 비활성화된다.** 명시적 null은 `"auto"`를 넣어줄 자동 주입을 건너뛰고, 파서를 절대 호출하지 않는 분기로 간다 | 클라이언트는 툴 호출이 없는 평문을 받고 **에러도 없다**. dorang은 리터럴 `null`을 `"auto"`로 정규화하거나 키를 생략해야 한다. 절대 그대로 전달하지 말 것 |
| 2.2 | **에러 `code`는 정수 HTTP 상태**이고, `type`은 Python 예외 이름(`BadRequestError`, `NotFoundError`)이다. 검증 실패에는 세 번째 형태가 또 있다 | OpenAI SDK는 문자열 code와 벤더식 type으로 분기한다. 매핑하지 않으면 오분기한다. COMPATIBILITY 7.1로 정규화 필요 |
| 2.3 | **`finish_reason`이 `abort` 또는 `repetition`일 수 있다** | OpenAI가 절대 내보내지 않는 값. COMPATIBILITY §4에 두 행이 필요하다 |
| 2.4 | **알 수 없는 요청 필드는 절대 거부되지 않는다** — 디버그 로그를 남기고 버린다 | dorang은 자신이 통과시키는 무엇에 대해서도 vLLM의 검증에 기댈 수 없다. 라이브 마이그레이션 위험이기도 하다: 구 `guided_json` 계열이 중첩 객체로 대체됐으므로 옛 이름을 계속 보내는 게이트웨이는 조용히 무시당한다 |
| 2.5 | **응답 리즈닝 필드는 `reasoning_content`가 아니라 `reasoning`이다** — 요청에서는 옛 이름도 여전히 받는다 | §10.2의 역매핑은 새 이름을 읽어야 한다. `--reasoning-parser`가 필요하며, 없으면 필드는 항상 null |
| 2.6 | **비스트리밍 응답은 null을 생략하지 않고 직렬화**해 vLLM 전용 필드 열 몇 개를 명시적 `null`로 내보낸다. 스트리밍은 생략하는 다른 체계를 쓴다 | 한 엔드포인트에 직렬화 체계가 둘. dorang은 compat egress 경로에서 vLLM 전용 필드를 각각 따로 제거해야 한다 |
| 2.7 | **`user`는 선언되고 명시적으로 무시**되며, `suffix`는 선언되고 400으로 거부되고, `best_of`는 조용히 버려진다 | 미지원 필드 셋에 대해 서로 다른 세 가지 동작 |
| 2.8 | **`parallel_tool_calls: false`는 생성을 제약하는 게 아니라 응답을 후필터링한다** | 모델은 여전히 여러 호출을 생성하고 vLLM이 첫 개 외에는 버린다 |
| 2.9 | **`logprob` 기본값이 `-9999.0`**, `-inf`를 대신하는 sentinel | 유효한 JSON `-Infinity`도 아니고 OpenAI 등가도 아니다. 평범한 float로 왕복시키지 말 것 |
| 2.10 | **모델 불일치는 400이 아니라 404다** | dorang에게 이것은 그대로 중계할 클라이언트 에러가 아니라 *라우팅* 실패 시그니처다 |
| 2.11 | **엔드포인트 플러그인은 마지막에 붙어 코어 라우트를 가릴 수 있다**, `/v1/chat/completions` 포함 | 서드파티 플러그인이 dorang이 호출하는 엔드포인트를 합법적으로 대체할 수 있다 |

### 컨텍스트 윈도우 초과 시그니처

§7.6의 `context_window` 폴백에는 400 시그니처가 필요하다. 문구는 **세 가지**가 있고, 둘은 OpenAI의
것과 구조적으로 같으며 하나는 다르다. 안정적인 부분 문자열 **`"maximum context length"`** 를 매칭하고
문장 전체는 쓰지 말 것 — 그리고 §1.1의 사전 계산 경로를 우선하고 시그니처는 backstop으로만 쓸 것.

---

## 3. 부하 신호

> **이 절은 R17의 명세이지, 돌고 있는 경로의 서술이 아니다.** dorang에는 메트릭 스크레이퍼가
> **없고** `providers[].metrics`는 로드에서 거부된다(CONFIG §6.2). `least_busy`는 dorang 자신의
> 실시간 용량 점유율로, `highest_tps`는 측정된 초당 출력 토큰으로 순위를 매기며 둘 다 폴이 필요 없다.

### 3.1 스크레이프할 가치가 있는 메트릭

모든 이름이 `{model_name, engine}`을 갖는다. `engine` 라벨은 data-parallel rank이며 **vLLM 자체
문서에는 빠져 있다** — 문서 생성기가 라벨을 내보내지 않는다.

| 신호 | 메트릭 |
|---|---|
| 실행 중 요청 | `vllm:num_requests_running` |
| 대기 요청 | `vllm:num_requests_waiting_by_reason{reason="capacity"}` |
| KV 캐시 사용률 | `vllm:kv_cache_usage_perc` |
| TTFT | `vllm:time_to_first_token_seconds_*` |
| prefix 캐시 | `vllm:prefix_cache_hits_total` / `vllm:prefix_cache_queries_total` |
| preemption | `vllm:num_preemptions_total` |

각각 에러가 아니라 **잘못된 라우팅 결정**을 낳는 함정 네 가지:

- **`kv_cache_usage_perc`는 퍼센트가 아니라 0–1 분수다.** 이름이 틀렸고, 문서 문자열은 "1이 100%
  사용"이라고 말한다.
- **맨 `vllm:num_requests_waiting`은 지연된(deferred) 요청까지 포함한다**, 큐잉된 것만이 아니다.
  큐 깊이 라우팅에는 `reason="capacity"` 시리즈를 쓸 것.
- **prefix 캐시 적중률 메트릭은 없다.** 두 카운터에서 계산할 것. 단위는 요청이 아니라 **토큰**이다.
- **`--disable-log-stats`를 주면 `/metrics`가 `vllm:` 시리즈 없이 200을 반환한다.** dorang은
  "엔드포인트 도달 가능, 시리즈 없음"과 "유휴"를 구별해야 한다 — 둘은 똑같아 보이면서 정반대를 뜻한다.

Ray 아래에서는 모든 이름의 `:`가 `_`로 치환된다. API 서버 프로세스가 여럿이면 gauge는 합이 아니라
한 프로세스의 값을 보고한다.

### 3.2 응답별 부하 헤더가 폴링보다 낫다

요청 헤더 `endpoint-load-metrics-format: JSON`을 보내면 vLLM이 KV 캐시 사용량과 대기 수를 담은
`endpoint-load-metrics` 응답 헤더를 반환한다. **서버 플래그 불필요.** `least_busy`에는 스크레이프보다
엄격히 낫다 — 낡았을 수 있는 gauge가 아니라 요청별 현재값이기 때문이다.

한계: 비스트리밍 응답 전용이고, `/v1/chat/completions`와 `/v1/completions`에서만 동작한다.
스트리밍 트래픽은 스크레이프로 폴백. 또 요청마다 INFO 로그를 남겨 dorang의 처리량 목표에서는 시끄럽다.

**값은 순수한 JSON이 아니다.** vLLM은 포맷 이름, 공백, 그다음 문서를 쓰므로 헤더를 통째로 넘긴
디코더는 실패한다:

```
endpoint-load-metrics: JSON {"named_metrics": {"kv_cache_utilization": 0.4}}
endpoint-load-metrics: TEXT named_metrics.kv_cache_utilization=0.4
```

`named_metrics`에는 float 값 필드만 들어가고, KV 수치의 이름은 거기서 `kv_cache_utilization`이다 —
같은 분수의 Prometheus 철자인 `kv_cache_usage_perc`가 아니다.

> **§3.1의 네 번째 함정이 여기서도, 위장한 채 살아남는다.** 이 헤더도 "메트릭 없음"과 "유휴"를
> 구별하지 않는다 — 오히려 스크레이프보다 *나쁘다*. 스크레이프는 시리즈 0개로 눈에 띄게 실패하지만
> 이쪽은 성공한다. vLLM은 보고를 이렇게 만든다:
>
> ```python
> kv_cache_utilization=(last_req_metrics.gpu_kv_cache_utilisation
>                       if last_req_metrics is not None else 0.0)
> ```
>
> 그래서 요청에 대한 메트릭이 없던 엔진이 빈 기계를 주장하는 잘 형성된 헤더를 돌려준다. 어떤
> 소비자든 정확한 `0.0`을 유휴가 아니라 미상으로 다뤄야 한다. dorang의 사용률 가격(DESIGN §8.6)에서
> 그 거부는 공짜다 — 점유율 0에서 가격 계수는 어느 쪽이든 1.0이다 — 그래서 그 읽기는 버려지고
> 행에는 아무도 하지 않은 측정 대신 "관측되지 않음"이 기록된다.

### 3.3 `/load`는 함정이다 — 쓰지 말 것

`{"server_load": N}`을 반환해 least-busy 라우터가 원하는 바로 그 신호처럼 보인다.
`--enable-server-load-tracking` 없이는 **영원히 `0`** 을 반환하며, 설정되지 않은 `0`은 진짜 유휴 서버와
구별되지 않는다 — 그러면서 그 라우터에게 **가장 매력적인** 값이다. 실패 모드가 단지 무정보한 게
아니라 능동적으로 해롭다. 플래그를 독립적으로 확인한 뒤에만 쓸 것.

---

## 4. Prefix 캐싱

자동 prefix 캐싱은 표준 dense 생성 모델에서 **기본 활성**이고, hybrid(Mamba 계열)에서는 꺼져 있다.
dorang은 켜져 있다고 가정해도 되지만 의존해서는 안 된다.

**dorang의 바이트 경계 prefix chain(§7.4b)은 설계 그대로 두어야 한다.** vLLM은 기본 16토큰
granularity로 *토큰 id*를 해싱하지만:

1. 그 경계에 맞추려면 핫패스에서 토큰화를 강제하게 된다 — §7.4b가 이미 거부한 바로 그 결함.
   그 거부가 이제 논증이 아니라 확증으로 뒷받침된다.
2. 실효 블록 크기는 attention 백엔드가 조용히 재작성할 수 있고 개발 모드 엔드포인트로만 읽힌다.
3. 맞춰도 어차피 얻는 게 없다. vLLM 자체 캐시 조회가 prefix-chained이므로, 바이트 단위로 동일한
   prefix는 동일한 토큰 prefix를 낳고 따라서 동일한 블록 해시를 낳는다 — dorang이 청크를 어디서
   잘랐든 무관하게.

### 아직 결정하지 않은 tenancy 트레이드오프

`cache_salt`는 vLLM의 KV 캐시를 분할하는 요청별 필드다. dorang의 `(tenant, group, session)`
격리(§7.4a)에 *엔진 내부에서* 실질적 힘을 주지만, 동시에 **공유 시스템 프롬프트의 테넌트 간 재사용을
파괴한다** — 직접적인 처리량 비용이다.

이것은 명시적 프로바이더별 옵션이어야 하며, **기본은 꺼짐**, 어느 방향으로든 dorang이 조용히 결정할
일이 아니다.

prefix를 조회·고정·프리페치하는 API는 없다. 관련 엔드포인트는 파괴적이고 개발 모드로 게이팅되며
실행 중 모든 요청을 선점할 수 있는 것 하나뿐이다.

---

## 5. 오퍼레이터 프로필

dorang이 권장하는 플래그와, 각각이 없을 때 조용히 깨지는 것:

| 플래그 | 없으면 |
|---|---|
| `--scheduling-policy priority` | `priority`가 받아들여지고 무시된다 (§1.2) |
| `--enable-prompt-tokens-details` | `cached_tokens`가 null이 되어 **§8의 비용 엔진이 캐시된 prompt 토큰을 정가로 과금한다** |
| `--enable-auto-tool-choice` + `--tool-call-parser` | `tools`가 400을 낳는다 |
| `--reasoning-parser` | `reasoning`이 항상 null |
| `--disable-log-stats`를 **주지 않기** | `/metrics`가 시리즈 없이 200을 반환 |
| `--enable-server-load-tracking` | `/load`를 쓸 때만 — §3.3 참조 |
| `--enable-request-id-headers` | `X-Request-Id`는 여전히 인바운드로 존중되지만 에코되지 않는다 |

`--enable-chunked-prefill`은 기본 활성이며, 끄면 대기 큐에서 head-of-line 블로킹이 생긴다.
`--max-num-partial-prefills`는 **존재하지 않는다** — 삭제된 엔진 세대의 것이며 dorang 설정에 등장해서는
안 된다.

**개발 모드 엔드포인트는 프로덕션 백엔드에 절대 호출하면 안 된다.** 그중에는 실행 중 모든 요청을
선점할 수 있는 prefix 캐시 리셋이 있다. dorang이 언젠가 vLLM 관리 표면을 갖게 된다면 별도로 활성화되는
오퍼레이터 capability 뒤에 있어야 한다.

---

## 6. 그 밖의 레버

- **`X-Request-Id`는 무조건 인바운드로 존중**되어 엔진 요청 id로 채택된다. 항상 보낼 것 — vLLM 자체
  로그와 트레이스로의 공짜 상관관계다.
- **`X-data-parallel-rank`** 는 요청을 특정 data-parallel 엔진에 고정해, dorang의 캐시 어피니티가
  내부 밸런싱을 믿는 대신 하나를 겨냥하게 한다. 잘못된 값은 조용히 무시되고, `/v1/responses`와 pooling
  엔드포인트에서는 받지 않는다.
- **LoRA admission이 priority를 이긴다.** 배치가 이미 최대 개수의 서로 다른 어댑터를 들고 있으면, cold
  어댑터에 대한 요청은 **priority와 무관하게** 밀린다 — hot 어댑터의 저우선 요청이 cold 어댑터의 고우선
  요청을 이긴다. LoRA 서빙 배포에서는 어댑터 어피니티가 §7.3 전략 체인에서 priority *앞에* 와야 한다.

---

## 7. 미검증

가정하지 않고 기록한다:

- 이 커밋의 릴리스 번호 — shallow clone, 태그 없음.
- 구체적인 백엔드별 블록 크기; 해당 서브트리가 체크아웃에 없었다.
- API 서버 프로세스가 여럿일 때 부하 헤더가 degrade하는지. 두 코드 경로가 서로 다른 메트릭 레지스트리를
  읽는 것은 명백하나 런타임 효과는 관찰하지 못했다.
- async 스케줄러가 priority 동작을 바꾸는지.

## 8. vLLM 자체 문서가 틀린 곳

전 구간에서 소스를 우선했다. 발견된 불일치:

| 주장 | 실제 |
|---|---|
| 비-priority 스케줄링에서 `priority != 0`은 에러 (동일한 필드 설명 다섯 곳에 명시) | 그런 검증은 존재하지 않는다 |
| `run_batch`는 chat completions만 지원 | 디스패치 테이블이 여섯 엔드포인트 계열을 덮는다 |
| 메트릭은 `model_name`만 갖는다 | `engine` 라벨도 있다 |
| `kv_cache_usage_perc`는 퍼센트 | 0–1 분수다 |
| swap 기반 preemption 모드 | recompute만; swap 플래그는 제거됐다 |
