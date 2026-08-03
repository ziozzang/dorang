# 적대적 보안 검토

대상: `dorang` — 프로바이더 크리덴셜을 보관하고, 테넌트별 인가와 예산을 강제하며, 신뢰할 수
없는 호출자와 유료 업스트림 사이를 중계하는 LLM 게이트웨이.

방법: `docs/DESIGN.md`(§2.4, §4.1, §5, §6, §7.4, §9, §10.6, §11.2),
`docs/COMPATIBILITY.md`(§2.0, §11), `docs/REVIEW.md`를 읽고, 이어서 `internal/auth`,
`internal/server`, `internal/admin`, `internal/store`, `internal/app`, `internal/shadow`,
`internal/backend`, `internal/batch`, `internal/capacity`, `internal/prefix`,
`internal/router`, `internal/quota`, `internal/config`, `internal/meter`, `cmd/`, 그리고
검토 도중 트리에 나타난 `internal/probe`의 일부를 읽었다. 저장소의 어떤 파일도 수정하지
않았다. 조작된 키를 담아 로컬에서 직접 작성한 파일에 대해 `dorangctl import config`를
오프라인으로 한 번 재현한 것 외에는 아무것도 실행하지 않았다. 어떤 외부 서비스도 호출하지
않았고 실제 크리덴셜은 하나도 쓰지 않았다.

이미 수정된 세 결함 — 게이트에서의 파서 불일치, 리다이렉트를 통한 크리덴셜 유출, 그리고
아무데도 연결되지 않은 제어 — 을 템플릿으로 삼았다. 세 형태 모두 재발한다. 세 번째가 가장
많이 재발한다.

> English (기준 문서): [SECURITY-REVIEW.md](SECURITY-REVIEW.md)

---

## 결함

### [CRITICAL] 배치 제출은 모델 허용목록도 예산 게이트도 없이 유료 모델 호출을 디스패치한다

- **위치:** `internal/app/batch.go:344-366`(생성), `internal/app/batch.go:302-309`
  (라우트), `internal/batch/scheduler.go:431`(행 디스패치), `internal/app/app.go:317`
  (마운트됨)
- **공격:** `models` 허용목록이 — 가령 `gpt-4o-mini`로 — 제한된 평범한 `sk-` 키의 소유자
  누구나. 그 키가 쓰도록 허용되지 *않은* 모델(`claude-opus-4`, 또는 카탈로그의 아무 이름)을
  행에 적은 JSONL 파일을 `POST /v1/files`로 올린 다음, 그 `input_file_id`로
  `POST /v1/batches`를 친다. 모든 행이 업스트림으로 디스패치된다.
- **영향:** 키별/유저별/팀별 모델 허용목록의 완전한 우회, 그리고 예산 상한의 완전한 우회 —
  그것도 운영자의 프로바이더 크리덴셜을 쓰는 경로에서. 배치 스케줄러는 용량 예약을 쥐고
  있으므로 레이트 한도에 걸려 무의미해지지도 않는다 — 파일 전체를 끝까지 돌린다.
- **근거:** `internal/server/server.go:411-416`의 게이트가 비추론 라우트에서 허용목록을
  참조하는 유일한 지점이다:

  ```go
  if rq.Principal != nil {
      if err := rq.Principal.Authorize(Access{Model: rq.Model, Route: rq.Path}); err != nil {
  ```

  `rq.Model`은 *배치 생성* 본문에 대한 `peekRequest`에서 오는데, 그 본문은
  `{"input_file_id","endpoint","completion_window","metadata"}`이다 — 최상위 `model` 필드가
  없으므로 `rq.Model == ""`이다. 그러면 `internal/auth/principal.go:156`이 검사를 통째로
  건너뛴다: `if a.Model != "" && !allowedIn(...)`. 두 번째 검사인 `Principal.AllowsModel`은
  `handleInference`(`internal/server/handlers.go:100`)에만 있고, `/v1/batches`는
  `handleInference`가 아니라 `a.handleBatchCreate`가 처리한다.

  생성 핸들러는 신원을 나르지만 그것에 아무것도 묻지 않는다:

  ```go
  out, err := a.Batch.Create(rq.Context(), batch.CreateRequest{
      InputFileID:      body.InputFileID,
      ...
      OwnerKeyID:       principalID(rq),
  })
  ```

  스케줄러는 행의 모델을 소유자에 대한 어떤 참조도 없이 *전역* 타깃 테이블에 대고
  해석한다(`internal/batch/scheduler.go:431`):

  ```go
  tgt, ok := s.cfg.Models.ResolveModel(row.Model)
  ```

  그리고 예산 게이트는 부재한다: `grep -n "udget" internal/batch/*.go internal/app/batch.go
  internal/app/batchstore.go`는 아무것도 반환하지 않는다. `budgetGate.reserve`는 정확히 한
  곳, `internal/app/dispatch.go:117`에서만 호출되는데 배치 실행기는 거기에 결코 도달하지
  않는다(`internal/app/batch.go:212-278`이 `dispatcher.Dispatch`를 우회해 직접
  디스패치한다).
- **수정:** `handleBatchCreate`에서, 그리고 이미 모든 JSONL 줄을 훑는 행
  검증기(`internal/batch/validate.go`)에서 한 번 더, `rq.Principal.AllowsModel(row.Model)`을
  통과하지 못하는 `model`을 가진 행을 거부한다. 그리고 배치 실행기에는 `dispatcher.Dispatch`가
  쓰는 것과 같은 `budgetGate.reserve`/`settle` 쌍을 배치의 `OwnerKeyID`를 키로 하여 준다.

---

### [HIGH] 주체별 레이트·동시성 한도 세 개가 저장되고, 임포트되고, 관리되고, 문서화된다 — 그리고 한 번도 강제되지 않는다

- **위치:** `internal/auth/principal.go:46-52`와 `:165-170`(검사),
  `internal/app/authn.go:131`과 `internal/server/server.go:412`(`auth.Access`를 만드는
  단 두 곳), `internal/app/build.go:95-97`(용량 배선)
- **공격:** 운영자가 키에 `rpm_limit: 60`을 건다(기존 게이트웨이에서 임포트했거나
  `/key/update`로). 그 키가 분당 60,000 요청을 낸다. `tpm_limit`과
  `max_parallel_requests`도 똑같다.
- **영향:** 키별·유저별·팀별 요청 레이트, 토큰 레이트, 동시성 상한은 존재하지 않는다.
  DESIGN §11.2는 "팀·유저·키 한도가 모두 적용되고 가장 엄격한 것이 이긴다"고 말하지만, 그중
  셋은 아무것에도 적용되지 않는다. 이 게이트웨이의 비용 통제는 예산 게이트(아래에 나오듯
  자체 결함이 있다)와 크리덴셜별 쿼터로 축소된다.
- **근거:** 강제 분기는 `auth.Access`의 필드 두 개를 읽는다:

  ```go
  if l.RPMLimit != nil && int64(a.ObservedRPM) >= *l.RPMLimit {
      return refuse(ReasonRateLimited, subject, "rpm")
  }
  if l.TPMLimit != nil && int64(a.ObservedTPM) >= *l.TPMLimit {
  ```

  `ObservedRPM`과 `ObservedTPM`은 테스트 밖 어디에서도 할당되지 않는다. 프로덕션의 두
  생성 지점은 이렇다:

  ```go
  // internal/app/authn.go:131
  err := p.p.Authorize(auth.Access{Now: p.now(), Model: a.Model, Route: a.Route})
  // internal/server/server.go:412
  rq.Principal.Authorize(Access{Model: rq.Model, Route: rq.Path})
  ```

  둘 다 두 카운터를 0으로 남기므로 비교는 `0 >= limit`이 되고, 양수 한도라면 언제나 거짓이다.
  `MaxParallel`은 더 나쁘다: `internal/auth/principal.go:50-52`는 "It is enforced by
  internal/capacity"라고 말하지만 `MaxParallel`은 `internal/capacity` 어디에도 나타나지
  않는다. 브로커의 주체별 상한은 오직 정적 YAML에서만
  온다(`internal/app/build.go:95-97`):

  ```go
  for name, l := range cfg.Capacity.Principals {
      c.Principals[name] = l.MaxConcurrent
  }
  ```

  키, 유저, 팀, 배포에 달린 `max_parallel_requests` 컬럼은 거기에 결코 도달하지 않는다.
- **수정:** (a) `internal/app/authn.go:131`에서 `internal/quota`의 키별 미터로
  `Access.ObservedRPM`/`ObservedTPM`을 채우고 `internal/app/dispatch.go`에서
  `Limits.MaxParallel`을 `capacity.Request`에 먹이거나, (b) `auth.Limits`와 admin/store
  스키마에서 세 필드를 삭제해서 일어나지도 않는 강제를 약속하는 것이 없게 한다. 셋 중 최악은
  그것들을 무력한 채로 출하하는 것이다.

---

### [HIGH] 팀 또는 유저 예산 상한이 키 단위로 강제되어, 한 팀 아래 N개의 키가 각각 상한 전액을 받는다

- **위치:** `internal/app/budget.go:176-205`(`principal.budget`),
  `internal/app/budget.go:102-103`(영속 키)
- **공격:** 어떤 팀에 `max_budget: 100 USD/month`를 준다. 그 팀의 키 열 개는 자기 키 수준
  예산이 없다. 각 키는 *팀의* 한도를 자기 상한으로 삼아 자기 영속 카운터에 대해 독립적으로
  예약한다. 팀은 1000 USD를 쓴다.
- **영향:** 팀과 유저 예산 상한 — 운영자가 한 부서를 묶으려고 설정하는 바로 그것 — 이 그
  아래 키 개수만큼 곱해진다. 키가 많은 팀에서는 사실상 무한하다.
- **근거:** `budget()`은 키·유저·팀에 걸쳐 한도의 *최솟값*을 취하는데, 방향은 옳다:

  ```go
  for _, l := range []*auth.Limits{&p.p.Key, p.p.User, p.p.Team} {
      if l == nil || l.MaxBudgetNanoUSD == nil { continue }
      if v := *l.MaxBudgetNanoUSD; !ok || v < limit { limit, ok = v, true }
      if period == "" { period = l.BudgetPeriod }
  }
  ```

  그러나 그 한도가 적용되는 카운터는 **key id**만을 키로 한다
  (`internal/app/budget.go:102-103`):

  ```go
  hold, err := g.ledger.Reserve(ctx,
      cluster.BudgetKey(budgetSubjectKind, p.KeyID(), window, g.now()), limit, amount)
  ```

  `budgetSubjectKind = "key"`이다(`internal/app/budget.go:33`).
  `internal/app/budget.go:173-175`의 주석은 성립하지 않는 안전 속성을 단언한다: "a user or
  team ceiling lower than the key's still binds the request, which is the direction that
  cannot fail open." 그것은 각 키를 팀 상한에 *따로따로* 묶으며, 그게 바로 N배로 fail open
  하는 것이다.
- **같은 함수의 부차적 결함:** `limit`은 주체들에 걸친 최솟값인데 `period`는 예산을 가진
  주체 중 *처음으로 비어 있지 않은* 기간이다. 일일 1 USD 예산을 가진 팀 아래 월간 10 USD
  예산을 가진 키는 `limit = 1 USD, period = monthly`가 된다 — 일일 상한을 월간 창에 적용하는
  것이며, 이는 `internal/auth/principal.go:36-41`이 문서화하는 "한도가 자기 기간 없이
  실려 다닌다"는 위험 그 자체다.
- **수정:** 최솟값으로 hold 하나를 잡을 게 아니라 구속하는 주체마다 hold을 잡는다. 상한을
  선언한 모든 주체에 대해 `BudgetKey("key", keyID, …)`, `BudgetKey("user", userID, …)`,
  `BudgetKey("team", teamID, …)`에 각각 *자기 한도와 자기 기간으로* 예약하고, 실패 시 전부
  함께 해제한다.

---

### [HIGH] 업스트림의 오류 메시지가 클라이언트용 봉투에 그대로 복사되어, COMPATIBILITY §11.3과 모순되고 프로바이더 크리덴셜을 노출한다

- **위치:** `internal/server/errors.go:269`, `:287`, `:302`, `:315`, `:358`;
  `internal/backend/errors.go`(`upstreamError` → `server.Normalize`)에서 도달한다
- **상태: 부분 종결.** 스크러버 쪽 절반은 이제 출하되는 경로 위에 있다. 이 글을 쓸 당시
  디스패치 경로는 `internal/app` 안에서 자체 HTTP 호출을 했고 스크러버가 없었다. 그 사본은
  삭제됐고 `internal/app`은 이제 `internal/backend`를 호출한다. `internal/backend`의
  `upstreamError`는 아웃바운드 크리덴셜 헤더에 실제로 실린 것을 수집하고(`collectSecrets`.
  크리덴셜 테이블이 아니라 헤더를 읽으므로, 자기가 소유하지 않은 코드가 붙인 OAuth 토큰도
  잡힌다) 무엇이든 렌더링되기 전에 메시지·네이티브 타입·code에서 그것을 지운다. 이 지적의
  *나머지* 절반은 그대로 성립한다: `Normalize`는 여전히 업스트림 자신의 메시지를 응답 본문에
  넣는데 COMPATIBILITY §11.3은 그러면 안 된다고 말하며, 구조화된 네 분기는 여전히
  무제한이다.
- **공격:** 두 변종이며 어느 쪽도 dorang에 대한 접근을 필요로 하지 않는다.
  1. 문제의 키를 401 본문에 되울리는 프로바이더. 여러 OpenAI 호환 서버가 그렇게 하고
     (`{"error":{"message":"Invalid API key: sk-…"}}`), OpenAI 자신도 부분적으로 가려진
     형태로 되울린다. 인증된 호출자라면 누구나 어떤 프로바이더 크리덴셜이 무효이거나
     폐기됐거나 회전 중일 때 요청 하나를 보내고, dorang의 401 본문에서 프로바이더의 메시지를
     곧바로 읽는다.
  2. 침해되었거나 적대적인 백엔드 — DESIGN §4.4가 일급 프로바이더 종류로 삼는 자체 호스팅
     vLLM/SGLang 노드를 포함해 — 는 그냥 아무 요청에나
     `{"error":{"message":"<the x-api-key header it just received>"}}`로 답하면 된다.
- **영향:** 유효한 `sk-` 키를 가진 임의의 테넌트에게 운영자의 프로바이더 크리덴셜(또는 그
  상당 부분의 접두사)이 유출된다. 이는 이미 수정된 리다이렉트 결함과 같은 부류 — 적대적
  업스트림이 응답 경로를 크리덴셜 채널로 바꾸는 것 — 이며, 응답 *본문*에 대해서는 닫히지
  않았고 패스스루 엔진의 응답 헤더에 대해서만 닫혔다.
- **근거:** COMPATIBILITY §11.3은 모호하지 않다:

  > 업스트림 자신의 `type`, `code`, 그리고 메시지는 원장에 기록되고
  > `x-dorang-native-error-type` / `x-dorang-native-error-code`로 드러난다. 응답 본문에는
  > **넣지 않는다**.

  `Normalize`는 모든 분기에서 그것들을 응답 본문에 넣는다:

  ```go
  e.Message = s              // errors.go:269  — SGLang bare-string envelope
  e.Message = in.Message     // errors.go:287  — OpenAI-nested and Anthropic
  e.Message = s              // errors.go:302  — FastAPI detail
  e.Message = probe.Message  // errors.go:315  — SGLang flat envelope
  e.Message = http.StatusText(status) + ": " + string(b)  // errors.go:358 — opaque
  ```

  그리고 `appendEnvelope`(`internal/server/errors.go:434`)이 클라이언트가 읽는
  `{"error":{"message":…}}` 안에 `e.Message`를 쓴다. 유일하게 경계가 있는 것은
  `opaqueError` 분기이며 `excerptLimit = 256`(`internal/server/errors.go:215`)이다 — 그리고
  거기 달린 주석("so that a stack trace or a credential echoed into a 500 page cannot be
  relayed wholesale")은 그 귀결에 대해 틀렸다: `sk-` 키는 51~164자이고 256바이트 안에 여유
  있게 들어간다. 구조화된 네 분기는 경계가 아예 없고, 오직
  `internal/app/dispatch.go:296`의 1 MiB 읽기에만 제한된다.
- **수정:** §11.3을 지킨다 — 업스트림 메시지는 `NativeMessage`(원장 + 헤더)에 넣고, 본문에는
  그 상황에 대한 dorang 자신의 정규 메시지를 낸다. `canonicalType`/`canonicalCode`가 이미
  나머지 두 필드에 대해 하는 것과 정확히 같게. 디버깅을 위해 발췌를 반드시 전달해야 한다면,
  기본값이 꺼져 있는 운영자 플래그 뒤에 두고, 쓰이기 전에 알려진 프로바이더 크리덴셜 값을
  전부 지운다.
- **이 코드베이스는 이미 이 규칙을 알고 있고 다른 곳에서는 올바르게 구현한다.**
  `internal/probe/doc.go:30-34`가 대놓고 말한다 — "a provider's error body can echo the
  key back, and wrapping it would put the key straight into the message… Every error and
  every recorded reason additionally passes a scrubber holding every secret the credential
  has presented" — 그리고 `internal/probe/scrub.go`는 질의 파라미터로 도착한 키의
  URL 이스케이프 철자까지 처리하는 동작하는 구현이다. `internal/probe/http.go`는 모든 오류
  지점(`:94`, `:111`)에서 그것을 적용하고 모든 읽기에 경계를 준다(`:98`, `:106`). 그 패키지는
  임포터가 없다. 출하되는 디스패치 경로에는 스크러버가 아예 없었다. 이것이 바로 그
  수정된-결함 형태의 재현이다: 올바른 코드는 존재하고, 실제로 도는 경로는 그것을 가진 경로가
  아니다.

  그 마지막 문장이 이 결함 부류 전체였고, 잡히기 전에 세 번째로 재발했다: `internal/backend`는
  `internal/app`에서 뽑아낸 추출물로 작성됐고, 거기서 네 건의 tool-call 결함이 수정됐으며,
  테스트도 통과했다 — 그런데 아무것도 그것을 임포트하지 않았으므로 프로덕션은 그 넷을 전부
  그대로 안고 있었다. 그 추출은 이후 완료됐고 `internal/app`의 사본은 삭제됐다. 두 사례가
  공유하는 교훈은, 패키지 테스트는 그 코드가 동작함을 증명할 뿐 무엇인가가 그것을 호출한다는
  것은 결코 증명하지 않는다는 것이다. 둘 다 잡아냈을 검사는 임포터 개수이며, 그다음은 수정을
  되돌렸을 때 깨지는 테스트다.

---

### [HIGH] 인증되지 않은 호출자가 요청마다 전역 쓰기 락 아래의 O(적재된 키 수) 맵 복사와 데이터베이스 질의 한 번을 유발할 수 있다

- **위치:** `internal/auth/authenticator.go:553-588`(`insert`, `mergeLocked`),
  `internal/server/server.go:381`을 거쳐 `internal/auth/authenticator.go:433-438`에서 도달
- **공격:** `Authorization: Bearer sk-<random>`을 달고 본문 없이
  `POST /v1/chat/completions`를 보낸다. 요청마다 무작위 접미사를 바꾼다. 인증은 본문을 읽기
  *전에* 돌므로(`internal/server/server.go:375-409`) 각 요청은 회선상 수백 바이트에
  불과하다.
- **영향:** 각 요청은 매번 새로운 조회 키를 가진 캐시 미스이므로 (a) `LoadByLookup` 저장소
  질의를 한 번씩 소모하고 — 네거티브 TTL 캐시는 서로 다른 키를 병합할 수 없다 — (b) 64번째
  서로 다른 미지의 키부터는 하나하나가 `a.mu`를 배타적으로 쥔 채 크리덴셜 스냅샷 맵 전체
  복사를 촉발한다. 적재된 키가 50,000개인 배포에서는 요청마다 50,000 엔트리 맵 할당과 복사가
  일어나고, 다른 모든 인증 미스에 대해 직렬화된다. 코드가 문서화한 완화책("a flood of
  unknown keys must not make every miss cost O(keys)")은 발동하지 않는다.
- **근거:** `insert`는 `mergeThreshold = 64`에 도달하면 overlay를 접는다:

  ```go
  a.overlay[l] = e
  switch {
  case len(a.overlay) >= maxOverlay:   // 8192
      a.overlay = nil
  case len(a.overlay) >= mergeThreshold:  // 64
      a.mergeLocked()
  }
  ```

  그런데 `mergeLocked`는 *긍정* 엔트리만 승격시키고 부정 엔트리는 새 overlay로 그대로
  돌려준다:

  ```go
  cur := *a.snap.Load()
  m := make(map[Lookup]*entry, len(cur)+len(a.overlay))
  for k, v := range cur { m[k] = v }        // O(loaded keys), every call
  keep := make(map[Lookup]*entry)
  for k, v := range a.overlay {
      if v.found { m[k] = v } else { keep[k] = v }
  }
  a.snap.Store(&m)
  a.overlay = keep                          // still >= 64 negatives
  ```

  미지의 키 홍수는 부정 엔트리만 만들어내므로 `len(a.overlay)`는 결코 64 아래로 다시 떨어지지
  않고, `mergeLocked`는 8192 엔트리 리셋에 이를 때까지 사실상 뒤따르는 모든 미스마다 돈다 —
  공격자 요청 8192건당 대략 8128번의 전체 스냅샷 복사다. 핫패스 역시
  `internal/auth/authenticator.go:425`에서 `a.mu.RLock()`을 잡으므로, 정상적인 미스가 쓰기
  측 뒤에 줄을 선다.
- **수정:** overlay에 승격 가능한 엔트리가 하나도 없으면 `mergeLocked`를 호출하지 않는다 —
  `insert`에서 `positives` 개수를 추적하고 `len(a.overlay)`가 아니라 그것으로 병합한다.
  부정 집합은 따로 제한한다(작은 링, 또는 자체 상한을 가진 두 번째 맵). 별개로, 미지의 키에
  대한 저장소 조회를 출처별로 레이트 한도를 걸거나 병합한다. 인증되지 않은 요청 하나당 질의
  하나는 그 자체로 SQLite에 대한 증폭 계수이기 때문이다.

---

### [HIGH] prefix 친화도 테이블이 테넌트 범위가 아니어서, 어떤 테넌트든 다른 테넌트의 프롬프트 접두사에 대한 바이트 단위 정확한 오라클과 라우팅 오염 프리미티브를 얻는다

- **위치:** `internal/prefix/chain.go:81-93`(`NewChain`), `internal/prefix/chain.go:169`
  (`Compute`), `internal/app/dispatch.go:210-215`(유일한 대화형 호출 지점),
  `internal/app/app.go:174`(프로세스 전역 테이블 하나), `internal/router/router.go:852-863`
  와 `:1191-1193`(조회와 기록), `internal/router/strategy.go:52-53`(그것이 라우팅을 결정한다)
- **공격:** 테넌트 A와 B가 모델 그룹 하나를 공유한다. 코딩 에이전트 클라이언트는 요청마다
  크고 바이트 단위로 동일한 시스템 프롬프트를 보내며, 그것은 4 KiB 첫 체크포인트 안에 충분히
  들어간다. B가 후보 접두사 바이트와 `X-Dorang-Detail: full`을 실어 요청을 보낸 뒤, 자기
  응답에서 `X-Dorang-Route-Reason: prefix_hit:depth=N`을 읽는다
  (`internal/app/dispatch.go:607` → `internal/server/headers.go:227-228`). 히트는 다른
  어떤 테넌트가 그 모델에 대해 최근에 정확히 그 바이트들을 보냈다는 뜻이다. 추측 바이트를
  바꿔가며 `depth`를 지켜보면 접두사를 한 바이트씩 걸어 나갈 수 있다. 별개로, `prefix_sticky`가
  기본 전략 체인의 첫 번째이고 `lowest_cost`보다 우선하므로, B는 A의 접두사를 먼저 보내서
  그 엔트리를 *심을* 수 있고, A의 다음 요청을 B의 요청을 처리한 배포에 고정시킬 수 있다.
- **영향:** 요청 접두사(시스템 프롬프트, 프로젝트 이름, 본문 앞쪽의 무엇이든)에 대한
  테넌트 간 확인 오라클, 그리고 다른 테넌트의 트래픽을 선택한 배포로 몰 수 있는 능력 —
  비용 부풀리기, 표적 성능 저하, 또는 공격자가 곧 실패할 것을 아는 배포로 몰아넣기.
- **근거:** 체인의 유일한 시드는 클라이언트를 향한 모델 그룹이다:

  ```go
  func NewChain(group string, baseSegment int) *Chain {
      ...
      c.state = sha256.Sum256([]byte(group))
  ```

  그리고 유일한 대화형 생성 지점은 모델 이름만 넘기고 그 외에는 아무것도 넘기지 않는다:

  ```go
  if st.prefixOn && st.chunk > 0 {
      c.rreq.Digests = prefix.Compute(rq.Model, body, st.chunk)
  }
  ```

  `prefix.Digest`는 `[16]byte`이고(`internal/prefix/table.go`) 키 타입에 테넌트 성분이
  아예 없다.
- **정직한 단서:** 구현은 설계와 일치한다. DESIGN §7.4b 1007행이 `h₀ = H(group_id)`를
  명시한다. 따라서 이것은 설계로부터의 이탈이 아니라 설계의 구멍이다 — 다만 "캐시 친화도
  키는 설계상 테넌트 범위다"라는 검토 지시서의 전제는 §7.4a(세션 stickiness)에 대해서만
  참이고 §7.4b에 대해서는 참이 아니다. 공유 게이트웨이에서는 둘에 같은 규칙이 필요하다.
- **수정:** 체인을 테넌트가 앞서는 성분으로 시드한다 — `h₀ = H(tenant ‖ 0x00 ‖ group)` —
  테넌트는 `internal/app/dispatch.go:214`에서 `rq.Principal.TeamID()`(없으면 `KeyID()`로
  폴백)에서 가져온다. 그때까지는 `X-Dorang-Route-Reason`의 prefix 상세를 억제하면 오라클은
  없어지지만 오염은 남는다.

---

### [HIGH] 세션 stickiness의 테넌트 범위는 구현되고 테스트됐으며 한 번도 채워지지 않는다

- **위치:** `internal/router/request.go:82-84`(필드),
  `internal/router/router.go:638-643`(키), `internal/app/dispatch.go:198-206`(유일한
  프로덕션 생성 지점)
- **공격:** 같은 `X-Dorang-Session` 값을 제시하는 두 테넌트 — 기본값이거나 예측 가능한 세션
  id, 또는 피해자의 것을 의도적으로 흉내 내는 공격자 — 가 같은 핀으로 충돌한다.
- **영향:** DESIGN §7.4a 925행이 이것이 깨뜨리는 속성을 그대로 진술한다: "키는
  `(tenant, group, session)`이며 테넌트가 최상위 성분이라 두 테넌트가 핀을 공유하지 않는다."
  출하된 바이너리에서 그 최상위 성분은 언제나 `""`다. 테넌트 B는 테넌트 A가 쥔
  `(deployment, credential)` 핀을 건네받을 수 있고 — §7.4a2는 이것을 최적화가 아니라
  상태를 가진 대화에 대한 *정확성* 제약이라 부른다 — 주어진 세션 id가 누군가에게 고정되어
  있는지도 알아낼 수 있다.
- **근거:** 키는 올바르게 만들어진다:

  ```go
  return stickyKey{tenant: req.Tenant, group: g.Name, session: req.Session}
  ```

  그리고 `Request.Tenant`는 트리 전체에서 정확히 한 곳에서만 할당된다:

  ```
  $ grep -rn "Tenant:" --include=*.go .
  testing/scenario/harness.go:421:		Tenant:        c.Tenant,
  ```

  `internal/app/dispatch.go:198-206`은 `Model`, `Principal`, `Session`, `AllowLossy`,
  `InputTokens`, `MaxOutputTokens`, `Stream`을 세운다 — 그리고 `Tenant`는 세우지 않는다.
  신원은 바로 거기 있다: `server.Principal`이 `KeyID()`, `UserID()`, `TeamID()`를 노출하고
  (`internal/server/deps.go:32-40`), 전부 `internal/app/authn.go:120-127`에서 채워진다.
- **수정:** 한 줄이다 — `internal/app/dispatch.go:198`의 `router.Request` 리터럴에
  `Tenant: tenantOf(rq.Principal),`을 추가하고, 배치 호출 지점
  (`internal/app/batch.go:141`)에도 똑같이 한다. principal을 실은 요청에서 그 필드가
  영값이면 깨지는 테스트를 추가한다.

---

### [HIGH] 패스스루 라우트는 모델 허용목록 없이 디스패치한다

- **위치:** `internal/server/passthrough.go:133`(`NeedsBody: false`),
  `internal/server/server.go:389-416`(게이트), `internal/server/handlers.go:100`(건너뛰는
  검사)
- **공격:** `passthrough.enabled: true`인 배포와 라우트
  `{ prefix: /anthropic, provider: anthropic-main, auth: dorang }`. `gpt-4o-mini`로 제한된
  키가 `{"model":"claude-opus-4",…}`로 `POST /anthropic/v1/messages`를 보낸다. 요청은
  인증되고, 라우트 허용목록은 검사되고, 모델 허용목록은 검사되지 않는다.
- **영향:** 설정된 패스스루 접두사 뒤로 닿을 수 있는 모든 모델에 대해 키별 모델 허용목록이
  우회되며, 운영자의 프로바이더 크리덴셜을 쓴다(`auth: dorang`에 대해
  `passthroughRoutes`가 그것을 붙인다, `internal/app/build.go:668-673`).
- **근거:** 모델은 `NeedsBody`를 선언한 라우트에 대해서만 peek 된다:

  ```go
  if rt.NeedsBody {
      ...
      model, stream, ok := peekRequest(b)
      rq.Model, rq.Stream = model, stream
  }
  if rq.Principal != nil {
      if err := rq.Principal.Authorize(Access{Model: rq.Model, Route: rq.Path}); err != nil {
  ```

  패스스루는 `NeedsBody: false`를 의도적으로 세우므로("step 3: relay without parsing,
  streaming") `rq.Model`은 `""`이고 `Limits.authorize`가 검사를 건너뛴다
  (`internal/auth/principal.go:156`). `servePassthrough`
  (`internal/server/passthrough.go:210-266`)는 principal에게 다시는 아무것도 묻지 않는다.
- **수정:** 정직한 선택지는 (a) 설정된 키 중 하나라도 비어 있지 않은 `models` 허용목록을
  가지고 있으면 패스스루 라우트 컴파일을 거부해서, 그 비호환이 조용한 우회가 아니라 시작
  시점 오류가 되게 하거나, (b) 콘텐츠 타입이 JSON인 패스스루 본문에 대해 경계 있는 peek을
  받아들이되 릴레이가 이미 적용하는 것과 같은 상한에 대고 `peekRequest`를 재사용하고, 그
  결과에 허용목록을 강제하는 것이다. (a)가 더 작고 fail closed 한다.

---

### [HIGH] 비스트리밍 업스트림 응답을 무제한 `io.ReadAll`로 읽는다

- **위치:** `internal/app/dispatch.go:332`
- **공격:** 적대적이거나 침해되었거나 그저 잘못 동작하는 백엔드 — 자체 호스팅 엔진을 포함해 —
  가 `200 application/json`에 수 기가바이트 본문을 실어 답한다. 거기로 라우팅되는 인증된
  호출자면 누구나 그 읽기를 촉발한다.
- **영향:** 본문 전체가 버퍼링되고, 그다음 `convertResponse`가 그것을 언마셜하고
  (`internal/app/dispatch.go:341`) 결과를 다시 마셜하므로, 동시 요청 하나당 최대 상주
  메모리가 응답 크기의 대략 3~4배가 된다. 이것은 전적으로 신뢰 경계의 업스트림 쪽에서
  유발되는 원격 OOM이다.
- **근거:** 같은 함수에서 오류 경로는 경계가 있고 성공 경로는 없다:

  ```go
  if resp.StatusCode >= 400 {
      body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))   // :296 — bounded
  ...
  body, err := io.ReadAll(resp.Body)                             // :332 — unbounded
  ```

  DESIGN §15.5는 본문 전체 버퍼링을 금지하고, §10.6은 패스스루 릴레이에 대해 그 경계를
  "구현 세부가 아니라 계약의 일부"로 만든다(`internal/server/passthrough.go:192-201`,
  `relayBufferSize`와 `usageScanLimit`). 경로는 둘, 규칙은 하나, 강제는 하나뿐이다.
  *요청* 본문에 대해서는 문서화된 프로세스 전역 리플레이 예산이
  있지만(`internal/server/server.go:25`) 응답 본문에 대해서는 아무것도 없다.
- **수정:** 설정 가능한 상한 — 기본값은 요청 상한 — 을 두고
  `io.ReadAll(io.LimitReader(resp.Body, cfg.maxResponseBytes))`를 쓰며, 한도에 걸리면
  `502 upstream_response_too_large`를 낸다.

---

### [MEDIUM] 요청 본문이 동시성 게이트보다 먼저 전부 버퍼링되므로, 키별 상한이 메모리를 한정하지 못한다

- **위치:** `internal/server/server.go:389-409`(버퍼), `internal/app/dispatch.go:103`
  → `internal/capacity/broker.go:381-385`(게이트, 하류)
- **공격:** 유효한 키 하나, 동시 연결 N개, 각각 32 MiB 상한 바로 아래 크기의 본문으로
  `POST /v1/chat/completions`.
- **영향:** 모든 연결의 본문이 키당 동시 32개 용량 검사에 닿기 전에 메모리로 읽히고, 그다음
  슬롯을 기다리며 줄 서는 요청은 최대 `MaxQueueWait`(기본 30 s) 동안 자기 버퍼를 쥐고 있다.
  키별 동시성 상한은 *디스패치된* 요청을 한정하지 *버퍼링된* 요청을 한정하지 않는다.
  `netutil.LimitListener`나 그에 상당하는 것이 없다(`cmd/dorang/main.go:173`은 평범한
  `net.Listen`이다). **상한 자체는 이제 설정 가능하다** — `server.max_body_bytes`(CONFIG §2)가
  `server.Options.MaxBodyBytes`에 도달하므로 운영자가 낮출 수 있다. 그것으로 어떤 운영자도
  낮출 수 없다고 했던 이 결함의 절반은 닫힌다. 입장(admission) 쪽 절반은 그대로 선다: 버퍼는
  여전히 동시성 게이트보다 먼저 잡히고, 상한을 낮추는 것은 동시 버퍼의 개수를 한정하는 것이
  아니라 연결당 비용을 줄일 뿐이다.
- **근거:** `serve`는 `internal/server/server.go:391`에서 본문을 읽고 `rt.Handler`는 `:418`에서야
  호출한다. 용량 획득은 `internal/app/dispatch.go:103`의 `st.router.Route` 안에 있고,
  `handleInference`는 그 읽기 이후에 거기 도달한다.
- **수정:** `rq.body.read` 이전에 저렴한 주체별 입장 토큰을 획득하고 요청이 끝날 때 반납한다.
  `server.max_body_bytes`를 설정으로 노출하는 것은 **완료**다
  (`TestConfiguredBodyCapIsHonoured`).

---

### [MEDIUM] `ReadTimeout`도 `IdleTimeout`도 없고, 본문 읽기에 데드라인이 없다 — slow-POST

- **위치:** `internal/server/drain.go:36-43`, `internal/server/request.go:256-308`
- **공격:** 인증된 호출자가 30 s `ReadHeaderTimeout` 안에 헤더를 완전하게 보내고
  `Content-Length: 33554432`을 선언한 뒤, 25초마다 1바이트씩 보낸다.
- **영향:** 핸들러 고루틴이 데드라인 없이 `r.Body.Read`에서 블록한다 — `ReadHeaderTimeout`의
  창이 지나고 나면 `ReadTimeout`이 설정돼 있지 않으므로 Go가 읽기 데드라인을 지운다. 여러
  연결에 걸쳐 반복하면 고루틴, 풀링된 `Request` 구조체, 파일 디스크립터를 무기한 붙잡아 둔다.
- **근거:**

  ```go
  hs := &http.Server{
      Handler: s,
      ReadHeaderTimeout: 30 * time.Second,
  }
  ```

  그 위의 주석은 "게이트웨이 자신의 요청 타임아웃이 핸들러를 한정한다"고 주장하지만,
  `internal/server/server.go:328-332`에서 만들어지는 요청 컨텍스트는 `Body.read`로 전혀
  이어지지 않는다. `Body.read`는 `*http.Request`와 `int64`를 받아 블로킹 `r.Body.Read`를
  도는 루프다. `DefaultRequestTimeout`은 6000 s(`internal/server/server.go:29`)이고 어차피
  여기에는 적용되지 않는다.
- **수정:** `http.Server`에 `ReadTimeout`과 `IdleTimeout`을 설정하고, `Body.read` 안에서
  `http.NewResponseController(w).SetReadDeadline`을 쓴다.

---

### [MEDIUM] `/v1/files`에는 파일당 상한만 있고 키별 쿼터도, 파일 개수 제한도, 만료도 없다

- **위치:** `internal/app/batch.go:319-328`(라우트), `internal/batch/files.go:37-61`
  (파일당 상한), `internal/app/batch.go:403-441`(업로드 핸들러)
- **공격:** 인증된 호출자가 `purpose=user_data`와 ~200 MiB 페이로드로 `POST /v1/files`를
  반복한다.
- **영향:** blob 디렉터리 아래의 디스크 고갈이며, 그것은 SQLite나 Postgres 저장소와 같은 볼륨
  위의 다른 모든 테넌트까지 함께 굶긴다. 업로드당 상한(`DefaultMaxFileBytes = 200 << 20`)은
  파일 하나를 한정한다. 개수도 총량도 아무것도 한정하지 않는다. `handleFileUpload`는
  `ExpiresAfter`를 한 번도 설정하지 않으므로 `rec.ExpiresAt`은 0으로 남고
  (`internal/batch/files.go:82-83`) 파일은 명시적으로 삭제될 때까지 남는다. `batch` 외의
  purpose는 JSONL 검증을 통째로 건너뛰므로(`internal/batch/files.go:63-68`) 페이로드가 형식에
  맞을 필요조차 없다.
- **수정:** `UploadFile` 안에서 검사하는 `OwnerKeyID`별 총 바이트·파일 개수 쿼터, 그리고 만료를
  지명하지 않은 업로드에 대한 기본 `ExpiresAfter`.

---

### [MEDIUM] 트랜스포트 에러 텍스트 — 내부 호스트명, 포트, URL — 가 디스패치 경로에서는 호출자에게 중계되는데, 패스스루 경로는 의도적으로 중계를 거부한다

- **위치:** `internal/app/dispatch.go:286-287` 대
  `internal/server/passthrough.go:373-376`
- **공격:** 인증된 호출자가 죽었거나, 느리거나, DNS가 실패하는 배포로 라우팅되는 요청을 보낸다.
- **영향:** 502 본문이 업스트림 URL과 트랜스포트 에러를 실어 나른다. 예:
  `upstream request failed: Post "http://vllm-a.internal.svc:8000/v1/chat/completions": dial
  tcp 10.0.3.14:8000: connect: connection refused`. 그것은 운영자의 내부 네트워크를 바깥에서
  지도 그리게 한다. Go의 `url.Error`는 박혀 있는 비밀번호는 가리지만 사용자명, 호스트, 포트,
  경로는 남긴다.
- **근거:** 두 경로가 같은 조건에 대해 정반대 규칙을 진술한다. 패스스루:

  ```go
  // The transport error text can name internal hosts and ports. It is
  // deliberately not relayed.
  return NewError(http.StatusBadGateway, TypeAPIError,
      "could not reach the upstream provider").WithCode("passthrough_unreachable")
  ```

  디스패치:

  ```go
  res.err = server.NewError(status, server.TypeAPIError,
      "upstream request failed: "+err.Error()).WithCode("upstream_unreachable")
  ```
- **수정:** `internal/app/dispatch.go:286`에서 패스스루 쪽 문구를 쓰고 자세한 에러는 `d.logf`로
  로그한다. 업스트림 본문 바이트를 인용할 수 있는 디코드 에러를 덧붙이는
  `internal/app/dispatch.go:460`에도 똑같이 적용된다.

---

### [MEDIUM] `x-dorang-native-error-type`이 검증되지 않고 길이 제한도 없는 업스트림 문자열로 설정된다 — 같은 패키지가 다른 곳에서 진술하는 규칙을 어기면서

- **위치:** `internal/server/errors.go:478` 대 `internal/server/server.go:469-478`
- **공격:** 적대적 백엔드가 `{"error":{"type":"a\r\n\r\n<injected>","message":…}}`를 반환한다.
  `json.Unmarshal`이 이스케이프를 실제 CR LF로 디코드하고, `canonicalType`이 그 문자열을
  `NativeType`으로 그대로 통과시키고, `WriteError`가 그것을 응답 헤더에 쓴다.
- **영향:** `net/http` 자신의 `ResponseWriter`에서는 이것이 무력화된다 —
  `Header.writeSubset`이 CR과 LF를 공백으로 바꾼다 — 그래서 오늘 당장 익스플로잇할 수는 없다.
  그러나 그 패키지는 100줄 떨어진 곳에서 바로 그 논리를 명시적으로 거부한다:

  ```go
  // The path is client-controlled and is about to become a header value.
  // net/http drops an invalid one at write time, but a response writer that
  // is not net/http's will not, so the check happens here — CRLF in a header
  // value is response splitting, and "the framework probably catches it" is
  // not a security argument.
  if safeHeaderValue(rq.Path) {
      rw.Header().Set(HeaderUnimplemented, rq.Path)
  }
  ```

  `WriteError`는 *업스트림*이 제어하는 값에 그런 검사를 전혀 적용하지 않는데, 그것은 검사를
  받고 있는 클라이언트 제어 경로보다 엄격하게 덜 신뢰되는 출처다. 그 값은 길이 제한도 없다 —
  1 MiB 에러 본문 읽기까지 — 그래서 백엔드가 1 MiB짜리 응답 헤더를 낼 수 있다.
- **수정:** `if safeHeaderValue(e.NativeType) { h.Set(HeaderNativeErrorType, clamp(e.NativeType, 128)) }`.
- **관련 잠재 위험:** `stampHeaders`는 맵 키에서 헤더 *이름*을
  `quotaPrefix+textproto.CanonicalMIMEHeaderKey(window)+quotaSuffix`로 만든다
  (`internal/server/headers.go:258-261`). `CanonicalMIMEHeaderKey`는 입력에 유효하지 않은
  바이트가 있으면 입력을 그대로 돌려준다. `Result.QuotaUsedPct`는 현재 무엇에 의해서도 채워지지
  않으므로(도달 불가 통제 절 참조) 이것은 불활성이다 — 그러나 언젠가 프로바이더가 보고한 윈도우
  이름에서 먹여지면 헤더 *이름* 주입이 되고, 그것은 위의 값 사례보다 나쁜 자리다.

---

### [MEDIUM] embeddings 릴레이가 엄격한 대소문자 충돌 필터 없이 클라이언트 본문을 업스트림으로 전달한다

- **위치:** `internal/app/dispatch.go:370-376`과 `:723-737`(`replaceModel`), 대
  `internal/canonical/strictjson.go:12-46`
- **공격:** `{"model":"allowed","Model":"other"}`로 `POST /v1/embeddings`.
- **영향:** `strictjson`은 대소문자 충돌 키를 이름을 바꾸거나 보존하는 대신 제거하며, 그 이유를
  정확히 진술한다: "패스스루 Extra 맵에 `Model`을 남겨 둔 어댑터는 그것을 다음 홉에 넘길 것이고,
  `encoding/json`으로 파싱하는 어떤 홉이든 — 다음 dorang, Go로 된 OpenAI 호환 서버 — 그 우회를
  한 고리 더 아래에서 재생성한다." embeddings 경로는 그 필터를 적용하지 않는다: `replaceModel`은
  `map[string]json.RawMessage`로 디코드하고, `obj["model"]`을 덮어쓰고, 다시 마셜한다 —
  `"Model"`을 그대로 업스트림까지 실어 나른다.
- **정직한 단서:** 나는 동작하는 익스플로잇을 구성하지 못했다. `json.Marshal`은 맵 키를 정렬하고,
  `model`의 모든 ASCII 대소문자 변형은 `model`보다 앞에 정렬되므로(대문자 바이트가 더 작다)
  올바른 키가 언제나 방출된 객체의 마지막에 오고, Go 업스트림에서는 마지막이 이긴다. 그 방어는
  우연이다 — 설계가 진술하는 규칙이 아니라 Go의 맵 키 정렬 순서와 이 필드 하나의 대소문자 충돌
  알파벳에 기대고 있다. 라이브 취약점이 아니라 하드닝 공백으로 보고한다.
- **수정:** `replaceModel`에서 다시 마셜하기 전에 본문을 `canonical.StrictBytes`(또는 그에
  상당하는 키 필터)에 통과시켜, 호출자의 원시 본문을 중계하는 그 하나의 경로가 그것을 디코드하는
  모든 경로와 같은 규칙을 지키게 한다.

---

### [MEDIUM] `dorangctl import config`가 프로바이더 키를 평문으로 stdout에 쓴다 — `SecretRef`의 가림이 자기 자신의 임베딩 태그에 의해 구조적으로 우회되기 때문에

- **위치:** `internal/config/secret.go:33`(`Inline` 필드), `internal/config/secret.go:88`
  (`MarshalYAML`), `internal/config/config.go:168`과 `:267`(`yaml:",inline"` 임베딩),
  `internal/config/import.go:471`, `cmd/dorangctl/config.go:114-120`
- **공격:** 기존 프록시에서 이관하는 운영자가 문서화된 워크플로
  `dorangctl import config old.yaml > dorang.yaml`을 실행한다.
- **영향:** 원본 파일의 모든 리터럴 `api_key`가 stdout에 평문 `key:` 값으로 다시 방출된다 — 새
  설정 파일로, 터미널 스크롤백으로, 이관이 스크립트로 돼 있다면 CI 로그로. DESIGN §4.1은
  "시크릿은 설정에 결코 나타나지 않는다"고 진술한다. 이것은 설정을 만들어 내는 것이 임무인 유일한
  도구이고, 시크릿을 거기에 쓴다.
- **근거:** 조작된 키를 담은 파일로, 네트워크 없이, 실제 바이너리에 대해 재현했다:

  ```
  $ go run ./cmd/dorangctl import config in.yaml
  ...
  credentials:
      - id: openai-key-1
        provider: openai
        key: sk-FAKE-NOT-A-REAL-KEY-000000000000000000
  ```

  그것을 막았어야 할 가림은 한 번도 돌지 않는다. `SecretRef.MarshalYAML`
  (`internal/config/secret.go:88`)은 의도적으로 참조만 방출한다 — 그러나 `SecretRef`를 쓰는 두
  곳 모두 그것을 `yaml:",inline"`으로 임베드하고(`internal/config/config.go:168`, `:267`),
  `gopkg.in/yaml.v3`는 인라인 임베드된 구조체의 노출 필드를 평탄화할 뿐 그 `MarshalYAML`을
  호출하지 않는다. 그 메서드의 문서 주석은 지나가며 이것을 인정한다("진짜 보증은 `resolve`가
  인라인 리터럴을 노출 필드 밖으로 옮긴다는 것") — 그리고 `ImportProxyConfig`는 정확히
  `resolve`를 결코 호출하지 않는 경로다. "의도적으로 검증되지 않기" 때문이다. 그러므로 서버 기동
  경로는 가림이 아니라 순서의 우연으로 안전하고, 그 순서를 건너뛰는 그 하나의 경로가 샌다.
- **정직한 단서:** 그 평문은 이미 운영자의 원본 파일에 있었으므로, 이것은 달리 보호되던 시크릿을
  노출하는 것이 아니라 시크릿을 전파하는 것이고, importer는 stderr에 경고하기는 한다
  (`internal/config/import.go:472-475`). 그것이 이것이 HIGH가 아니라 MEDIUM인 이유다. 흥미로운
  것은 구조적인 쪽이다: 존재하지만 한 번도 발동되지 않는 가림은, 존재하지만 한 번도 호출되지 않는
  검사와 같은 결함 부류다.
- **수정:** `credential()`이 `SecretRef{Env: suggestedEnvName}`을 방출하게 하고 리터럴 값은
  운영자가 배치하도록 `export FOO=…` 블록으로 stderr에 출력해, 생성된 설정이 시크릿을 결코 담지
  않게 한다. 그것이 안 되면, `Credential`에 `,inline`에 기대는 대신 `SecretRef.MarshalYAML`로
  넘겨 호출하는 명시적 `MarshalYAML`을 주고, 해결되지 않은 인라인 시크릿을 쥔 `Config`를
  마셜해서 출력에 평문이 없음을 단언하는 테스트를 추가한다.

---

### [MEDIUM] 최상위 키에 이스케이프된 바이트가 하나라도 있으면 라우팅 핫패스에서 본문 전체의 완전 언마셜이 강제된다

- **위치:** `internal/server/peek.go:32`, `:108-142`
- **공격:** 모든 요청이 이스케이프된 문자 하나를 가진 무관한 최상위 키를 포함시킨다. 예:
  `{"x":0,"model":"gpt-4o-mini","messages":[…32 MiB…]}`.
- **영향:** `sawEscapedKey`는 `model`이나 `stream`을 철자할 수 있는 키만이 아니라 *아무* 이스케이프된
  키에 의해 세워지고, 그러면 폴백이 본문 전체에 대해
  `json.Unmarshal(b, &map[string]json.RawMessage)`를 돌린다. 호출자가 단 하나의 요청도 빠짐없이
  본문 전체 JSON 디코딩을 강제할 수 있다 — 정확히 DESIGN §15.2.2가 "요청을 어디로 보낼지
  정하기도 전에 게이트웨이가 할 수 있는 가장 비싼 단 하나의 일"이라고 말하는 작업이다. 32 MiB
  상한에 한정되므로 이것은 메모리 고갈이 아니라 CPU 증폭이다.
- **근거:** 플래그는 어떤 이스케이프에든 무조건 세워지고(`internal/server/peek.go:55`) 폴백은 그
  플래그만으로 진입된다(`:108`).
- **수정:** *디코드된* 키가 `model`이나 `stream`일 수 있을 때만 `sawEscapedKey`를 세운다 — 가장
  싼 올바른 검사는 길이 검사에 더해, 이스케이프된 길이가 5바이트나 6바이트 결과를 낼 수 있는
  키만 언이스케이프하는 것이다.

---

### [MEDIUM] 받아들여지는 인증 헤더 목록이 서로 독립적으로 둘 있고, 교차 검사가 없다

- **위치:** `internal/auth/header.go:33-40`(`accepted`, `Extract`가 *인증하는 데* 쓴다)과
  `internal/server/headers.go:33-40`(`authHeaders`, `StripAuthHeaders`가 *전달 전에 스트립하는 데*
  쓴다)
- **공격:** 오늘은 없다 — 두 목록은 현재 같은 여섯 이름을 담고 있고, 둘 다 대소문자를 무시하고
  매치한다.
- **영향:** 두 목록은 따로 유지되고 그것들이 일치한다고 단언하는 것이 아무것도 없다.
  `internal/server/headers.go`에 대응하는 수정 없이 `internal/auth/header.go`에 일곱 번째
  받아들여지는 헤더를 추가하면 *호출자를 인증하고* 그다음 *프로바이더로 전달되는* 헤더가 생긴다 —
  COMPATIBILITY §7.3이 막으려고 존재하는 그 클라이언트 크리덴셜 유출이다.
  `internal/auth/header.go:124`(`auth.Strip`)에는 테스트 아닌 호출자가 아예 없다. 서버는 자기
  사본을 쓰고, 그것이 둘이 드리프트할 수 있게 된 경위다.
- **근거:** `grep -rn "auth\.Strip"`은 정의만 돌려준다.
  `internal/server/handlers_test.go:33`은 `len(AuthHeaders()) != 6`을 단언하지만 두 패키지의
  목록을 비교하지는 결코 않는다.
- **수정:** `internal/server/headers.go`의 사본을 지우고 서버가 `auth.Headers()`/`auth.Strip`을
  호출하게 하거나, `auth.Headers()`와 `server.AuthHeaders()` 사이의 집합 동일성을 단언하는
  테스트를 추가한다. 목록 하나이거나, 둘일 때 실패하는 테스트이거나.

---

### [MEDIUM] 패스스루와 WebSocket 릴레이가 업스트림 `Set-Cookie`를 dorang 자신의 오리진으로 전달한다

- **위치:** `internal/server/passthrough.go:415-423`(`copyResponseHeaders`),
  `internal/server/websocket.go:160-171`(`writeSwitchingProtocols`)
- **공격:** 패스스루 접두사 뒤의 침해된 백엔드가 `Set-Cookie: dorang_admin_ui=…; Path=/`로
  응답한다.
- **영향:** `copyResponseHeaders`는 모든 업스트림 헤더를 복사한 다음 hop-by-hop 헤더와 인증
  헤더를 스트립한다 — `Set-Cookie`는 어느 쪽 집합에도 없다. 따라서 업스트림이 dorang의 오리진에
  임의의 쿠키를 설정할 수 있고, 여기에는 운영자 UI의 세션 쿠키를 덮어쓰거나 지우는 것도 포함된다.
  세션 *고정*은 통하지 않지만(세션 id는 서버 측에서 발행되어 로컬 맵에서 조회된다,
  `internal/admin/ui.go:417-457`), 그래서 구체적 영향은 탈취가 아니라 운영자 UI 세션의 거부와
  일반적인 쿠키 주입이다. 관리 표면이 언젠가 같은 오리진에 마운트되고 *그 위에* 변경을 일으키는
  엔드포인트를 갖게 되면 실질적으로 훨씬 나빠진다.
- **수정:** `copyResponseHeaders`의 응답 측 스트립 집합에 `Set-Cookie`(그리고 `Set-Cookie2`)를
  추가한다. §10.6의 규칙 4가 인증 헤더에 이미 적용하는 것과 같은 논리다: 응답으로 상태를
  흘리는 백엔드는 그것이 중계되게 해서는 안 된다.

---

### [LOW] `shadow.Options`가 참조 게이트웨이의 크리덴셜을 가려지지 않은 필드에 담는다

- **위치:** `internal/shadow/options.go:64-66`
- **공격:** 오늘은 없다.
- **영향:** `ReferenceKey string`은 `String`, `GoString`, `Format`, `MarshalJSON` 재정의가 하나도
  없는 구조체의 평범한 필드다. 트리의 다른 모든 시크릿 보유 타입은 모든 `fmt` 동사 아래에서
  스스로를 가린다 — `auth.Config`(`internal/auth/authenticator.go:114-120`), `auth.Hasher`
  (`internal/auth/scheme.go:217-223`), `auth.Token`, `auth.OAuthConfig`,
  `auth.OAuthCredential`(`internal/auth/oauth.go:318`), `auth.OAuthManager`
  (`internal/auth/oauth.go:715-725`), `config.SecretRef`(`internal/config/secret.go:79-85`).
  이것은 그러지 않는다. 모든 호출처를 확인했고 `Options`를 포맷하는 곳은 없으므로 잠재적이다 —
  그러나 이 부류에서 단연 가장 약한 고리이며, 미래의
  `logf("shadow: starting with %+v", opts)` 하나면 그것은 로그 안의 크리덴셜이 된다.
- **수정:** 세 줄. 트리의 나머지가 이미 쓰는 패턴 그대로.

---

### [LOW] 라우트 테이블이 인증 이전에 열거된다

- **위치:** `internal/server/server.go:362-371`, `:469-487`;
  `internal/admin/api.go:252-279`
- **공격:** 인증되지 않은 호출자가 경로를 찔러 보고 상태 코드와 헤더를 읽는다.
- **영향:** `serve`는 `501 route_unknown` / `501 route_not_implemented`(요청된 경로가
  `X-Dorang-Unimplemented`에 그대로 되울린다)와 `Allow` 헤더를 단 `405`를 돌려주는데, 전부
  `cfg.auth.AuthenticateHeader`에 닿기 전이다. 관리 API도 똑같이 한다 — 라우트 조회, `OPTIONS`
  처리, 메서드 디스패치가 모두 `a.authenticate(r)`보다 앞선다. 호출자는 크리덴셜 없이 어떤
  패스스루 접두사가 설정돼 있는지, 어떤 라우트가 존재하는지, 그것들이 어떤 메서드를 받는지 알아낸다.
- **수정:** 공개가 아닌 라우트에 대해 405/501로 답하기 전에 인증하거나, 인증되지 않은 호출자에게는
  둘을 정보를 주지 않는 상태 코드 하나로 접는다. 어느 쪽이든 가치는 낮다. 완전성을 위해 기록한다.

---

### [LOW] 중계된 WebSocket에는 타임아웃도, 계측도, 키별 한도도 없다

- **위치:** `internal/server/websocket.go:36-38`, `:49-157`, `:178-194`
- **공격:** 패스스루 라우트에 `AllowWebSocket`이 켜진 상태에서, 인증된 호출자가 업그레이드를 많이
  열고 붙잡고 있는다.
- **영향:** "업그레이드가 완료되고 나면 타임아웃이 없다"는 의도된 것으로 진술돼 있다. 릴레이
  하나가 고루틴 둘, 연결 둘, 32 KiB 버퍼 둘, 그리고 결코 감소하지 않는 `inflight` 카운트 하나를
  쥐고, `rq.Result.Tokens`는 결코 설정되지 않으므로 아무것도 계측되거나 과금되지 않는다. 드레인
  중에 `waitInFlight`는 0에 닿을 수 없고 유예는 언제나 만료된다.
- **수정:** `pipe`의 양쪽 절반에 유휴 데드라인, 그리고 동시 릴레이에 대한 주체별 상한.

---

## 존재하지만 도달할 수 없는 통제

이 부류는 이제 한 번을 훨씬 넘게 나타났다. 이 코드베이스에서 지배적인 결함 형태이며, 네 등급으로
온다.

### 1. 관리 컨트롤 플레인 전체가 한 번도 마운트되지 않는다

`internal/admin` — 파일 20개, 대략 7,000줄, 유닛 테스트가 두텁다: 키 발급, `/key/block`과
`/key/unblock`, 예산, 유저, 팀, 지출 리포트, 감사 추적, `/admin/config/reload`, 그리고 세션
처리를 갖춘 내장 운영자 UI. 그것에는 **트리 어디에도 임포터가 하나도 없다**:

```
$ grep -rln "dorang/internal/admin" --include=*.go .
(no output)
$ grep -rn "admin\.New(" --include=*.go .
(no output)
```

`cmd/dorang/main.go`는 `*app.App`을 만들어 그것을 서빙한다. `internal/app/app.go`는
`internal/admin`을 한 번도 임포트하지 않는다. 살아 있는 라우트 집합은
`internal/server/handlers.go:baseRoutes()`에 `internal/app/batch.go:batchRoutes()`를 더한
것이다(`internal/app/app.go:317`). `/key/`, `/user/`, `/team/`, `/model/`, `/budget/`,
`/spend/`, `/global/spend/`는 `internal/server/server.go:492-497`에 `501
route_not_implemented`로 답하는 *예정된* 접두사로 나열돼 있다.

운영상의 귀결은 범주로 말하기보다 있는 그대로 말할 값어치가 있다: **유출된 API 키를 API를 통해
폐기할 방법이 없다.** 문서화된 사고 대응 절차 — `POST /key/block` — 를 따르는 운영자는 501을
받는다. 남은 레버는 `api_keys`에 직접 `UPDATE`를 날리고 그 뒤 API가 역시 발동할 수 없는 캐시
무효화를 하는 것, 아니면 프로세스 재시작뿐이다.

따라서 그 패키지 안에서 찾은 모든 것은 라이브가 아니라 잠재이며, 위의 결함이 아니라 여기에
보고한다:

- 라우트 디스패치, `OPTIONS` 처리, 405/501 응답이 전부 `a.authenticate(r)`보다 앞선다
  (`internal/admin/api.go:252-281`).
- 인가는 팀 범위가 없는 전역 비트 하나다 — 마스터 크리덴셜, 또는 역할이 `knownRoles`에 있는 아무
  유저(`internal/admin/api.go:300-320`, `internal/admin/users.go:90-96`). 팀 관리자라는 개념이
  없다.
- 모든 목록·정보 엔드포인트가 대상 id를 인증된 주체가 아니라 **요청에서** 가져온다:
  `spendLogs`(`internal/admin/spend.go:299-309`), `teamInfo`
  (`internal/admin/teams.go:188-199`), `keyInfo`/`keyList`
  (`internal/admin/keys.go:392-543`). 앞 항목과 합치면, 이것을 있는 그대로 마운트하는 것은 모든
  `admin_viewer`에게 모든 테넌트의 지출·키·예산에 대한 읽기 권한을 준다.
  `internal/admin/api.go:301-303`의 주석은 부재하는 권한 모델을 숙고된 선택으로 틀 지운다. 그것은
  모든 관리자가 완전 관리자인 동안에만 옹호 가능하고, `admin_viewer`가 함의하는 바는 그것이
  아니다.
- `admin.Hasher.Label`(`internal/admin/deps.go:163-173`)은 키 표시 라벨에 대한 비가역성 계약을
  문서화하면서 **프로덕션 구현이 없다** — 테스트 가짜뿐이다
  (`internal/admin/fakes_test.go:105-107`). 연결된 등가물인
  `store.LabelFor`/`labelFromLookup`(`internal/store/keys.go:138-151`)은 올바르다: 라벨을
  시크릿 자신의 문자가 아니라 조회 다이제스트의 앞 16진수 8자에서 파생하며, DESIGN §2.4가
  경고하는 기존 게이트웨이의 실수를 명시적으로 지목하는 주석을 달고 있다.

연결을 먼저 고치고, 그다음 위의 세 지적을, 그 순서로. 그것들 없이 연결만 붙이면 같은 커밋에서
잠재 결함 셋이 라이브 결함으로 바뀐다.

### 2. 어떤 강제에도 닿지 않는 강제 필드

| 통제 | 정의 | 읽는 곳 | 결과 |
|---|---|---|---|
| `Limits.RPMLimit` | `internal/auth/principal.go:47` | `principal.go:165`뿐이고, `Access.ObservedRPM`은 한 번도 대입되지 않는다 | 키/유저/팀별 요청 레이트가 무제한 |
| `Limits.TPMLimit` | `internal/auth/principal.go:49` | `principal.go:168`, 같음 | 키/유저/팀별 토큰 레이트가 무제한 |
| `Limits.MaxParallel` | `internal/auth/principal.go:52` | 아무것도 없음 — 그 식별자는 `internal/capacity`에 나타나지 않는다 | 주체별 동시성 상한이 무제한. 용량이 그것을 강제한다는 문서 주석은 거짓이다 |
| `capacity.{global,provider_groups,credential_groups,models,principals}.{rpm,tpm,max_queue,max_queue_wait}` | `internal/config/config.go:193-198`, `:235-241`, `validate.go:388-407`에서 검증 | `MaxConcurrency`/`MaxConcurrent`만 복사된다(`internal/app/build.go:95-107`). `capacity.Config`에는 그런 필드가 없다 | 모든 용량 축의 상한 네 가지가 깔끔하게 로드되고, 아무것도 경고하지 않고, 아무것도 강제하지 않는다 |
| `providers[].usage_probe.*` | `internal/config/validate.go`에서 검증 | ~~`quota.NewRegistry`와 `Meter.AttachTracker`에 테스트 아닌 호출자가 없음~~ → `internal/app/usageprobe.go` | ~~`usage_probe.enabled: true`는 받아들여지고 불활성이다. 모든 쿼터 결정이 조용히 로컬 전용으로 돌아, dorang 바깥에서도 쓰이는 크리덴셜의 사용량을 과소평가한다(DESIGN §6.2)~~ **2026-07-29 종결.** 활성화된 프로바이더마다 프로버 하나, 크리덴셜마다 게이트할 규칙을 가진 `quota.Tracker` 하나, 요청 경로 바깥에서 폴링한다. 프로버가 존재하지 않는 `fetcher`는 기동 거부다. `internal/app`의 `TestProviderReportedQuotaReachesTheMeter`가 로컬 트래픽이 전혀 없는 상태에서 프로바이더가 보고한 수치를 그 크리덴셜 자신의 거부까지 몰고 간다 |
| `router.Request.Tenant` | `internal/router/request.go:82-84` | `router.go:642`. 대입은 `testing/scenario/harness.go:421`에서만 | 세션 핀이 테넌트를 가로질러 공유된다(위의 결함) |
| 배치 경로의 `budgetGate` | `internal/app/budget.go` | `internal/app/dispatch.go:117`뿐 | 배치 지출에 예산이 없다(위의 결함) |
| `Result.QuotaUsedPct` | `internal/server/deps.go:327-349` | `headers.go:258` | `x-dorang-quota-*-used-pct` 헤더가 한 번도 방출되지 않는다. 언젠가 프로바이더가 보고한 윈도우 이름에서 먹여지면 잠재적 헤더 *이름* 주입 |
| `config.KeyRotation.Strategy` | `internal/config/validate.go:413-414`에서 검증 | `internal/config` 밖에서는 아무것도 없음 | 키 풀 로테이션 전략이 불활성. 인가가 아니라 분배에 영향 |

이 중 둘은 그 공백이 어떻게 살아남았는지에 대해 한마디 할 값어치가 있다. RPM/TPM 검사는 *유닛
테스트가 있다* — `internal/auth/auth_test.go:478-479`가 `a.ObservedRPM, a.ObservedTPM = 10, 100`
을 직접 주입해 그것을 발화시킨다 — 반면 `internal/app/authn_test.go`에는 두 필드 어느 쪽에 대한
참조도 없다. 테스트는 입력이 주어졌을 때 검사가 동작함을 증명하고, 그 입력이 도착한다는 것은
아무것도 증명하지 않는다. 그것은 이미 고쳐진 예산 버그가 가졌던 것과 같은 테스트 위상이며, 강제하는
함수를 고립시켜 단언하는 대신 통합 테스트가 종단 간 강제를 단언하기 전까지 이 결함을 계속 만들어
낼 것이다.

### 3. 호출자가 없는 서브시스템 통째

- **`internal/auth`의 OAuth 서브시스템.** `NewOAuthCredential`, `OAuthManager`, 싱글플라이트
  갱신, 원자적 토큰 저장소 쓰기, 401 백오프 게이트 — DESIGN §11.2b가 여든 줄을 들여 명세하는
  모든 기전 — 은 `internal/auth`와 그 테스트 바깥에 호출자가 없고, `internal/config.Credential`
  에는 그것을 켤 수 있는 필드가 없다. `internal/app/build.go:129-166`(`collectCredentials`)은
  정적 `Key.Value()`만 읽는다. OAuth 크리덴셜은 설정할 수 없으므로, §11.2b는 바이너리에 없는
  기능을 서술한다.
- **`internal/backend`**(`client.go`, `credential.go`, `endpoint.go`, `provider.go`) — 자기
  자신 밖에 임포터가 없다.
- **`internal/probe`** — git에서 추적되지 않는다(`?? internal/probe/`). 따라서 이것은 결함이
  아니라 진행 중인 작업이다. 트리에서 가장 좋은 시크릿 취급 코드인데 아무것도 그것을 쓰지 않기에
  적어 둔다(위의 §11.3 결함 참조).
- **`internal/luaext`** — 뼈대뿐, 임포터 없음. `extensions.lua.*`는 검증되고 불활성이다.
- **`quota.Ranker` / `Meter.Allowance`**(`internal/quota/urgency.go:262`, `:353-391`) —
  DESIGN §7.5a(c)의 만료 임박 쿼터 우선. 호출자는 `urgency_test.go`에만 있다. 보안 통제는
  아니다. 비용은 필요보다 조용히 높은 청구서다.
- **`store.ImportKeys`**(`internal/store/import.go:236-296`) — 식별자 위생(`validIdent` +
  `quoteIdent`)은 올바르게 만들어졌고 실제 호출자에 의해 한 번도 행사된 적이 없다. 어떤 CLI
  서브커맨드나 HTTP 라우트도 거기 닿지 않기 때문이다. `opts.Table`이나 `src *sql.DB`가 플래그나
  요청 값이 되는 순간 다시 감사할 것.
- **`quota.Budget`**(`internal/quota/budget.go`) — 프로덕션 호출자가 없고, 이것은 *올바르다*:
  내구성 있는 `cluster.Ledger`가 의도적으로 그것을 대체했고, 그것은
  `internal/app/dispatch.go:117-149`에 제대로 연결돼 있다. `internal/quota/doc.go:87-106`이
  여전히 그것을 "그" 예산 메커니즘으로 서술하고 있어 다음에 이 일제 점검을 하는 사람을 오도할
  것이므로 적어만 둔다.

### 4. 두 번 구현됐고 한 벌만 도는 규칙

- `auth.Strip`(`internal/auth/header.go:124`)에는 테스트 아닌 호출자가 없다. 실제로 도는
  스트리핑은 별개 구현인 `server.StripAuthHeaders`(`internal/server/headers.go:55`)를, 따로
  유지되는 목록 위에서 쓴다. 두 패키지의 문서 — `internal/auth/doc.go:9`,
  `internal/auth/authenticator.go:352-353`, `internal/auth/oauth.go:362` — 는 `auth.Strip`을
  전달 지점에서 호출되는 함수로 서술한다. 아니다. 그 주석들을 믿고 `auth.Strip`에 가한 변경은
  아무 일도 하지 않을 것이다. 두 헤더 목록에 대한 MEDIUM 결함을 볼 것.
- 네 개의 별개 `http.Client` 생성자가 각각 독립적으로 `CheckRedirect`를 설정한다:
  `internal/server/passthrough.go:147`, `internal/app/upstream.go:115`,
  `internal/shadow/reference.go:54`, `internal/backend/client.go:19`, 그리고
  `internal/probe/http.go:22`의 다섯 번째. 다섯 모두 현재 올바르다. 그 보안 속성은 한 곳에
  담기는 대신 다섯 번 되풀이해 진술되며, 그것이 최초의 리다이렉트 결함이 가능했던 방식이고 여섯
  번째가 그렇게 될 방식이다.
- `SecretRef.MarshalYAML` 대 `SecretRef.resolve`: 같은 불변식에 대한 두 메커니즘, 그리고 독자가
  하중을 지고 있으리라 기대할 쪽이 한 번도 돌지 않는 쪽이다(`dorangctl import config` 결함 참조).

---

## 검토자가 검증할 수 없었던 것

누락으로 암시하는 대신 있는 그대로 적는다.

- **검토 도중 작업 트리가 바뀌었다.** `internal/probe`(파일 일곱 개, ~55 KB)는 시작할 때
  `internal/`을 열거했을 때는 존재하지 않았고, 끝날 무렵에는 추적되지 않은 채 거기 있었다.
  `doc.go`, `scrub.go`, 그리고 `http.go`의 에러·읽기 지점은 읽었다. `probe.go`(26 KB),
  `decode.go`, `deepseek.go`, `zai.go`는 읽지 **않았다**. 그 패키지에 대한 결함은 전부
  부분적이며, 다른 파일도 내가 눈치채지 못한 채 발밑에서 움직였을 수 있다.
- **CLI 재현 하나를 빼면 어떤 코드도 실행하지 않았다.** 조작된 키를 담은 로컬 작성 파일에 대해
  오프라인으로 `go run ./cmd/dorangctl import config`를 빌드해 실행했고, 그것은 설정 import
  결함을 확인하기 위해서였다. 그 외에는 아무것도 실행하지 않았다: 테스트 스위트도, 퍼저도, 서버
  인스턴스도, 살아 있는 게이트웨이에 대한 요청도, 외부 서비스에 대한 호출도 없다. 다른 모든
  결함은 소스와 문서를 읽어 도출한 것이다. 공격 경로는 제어 흐름에서 구성한 것이지 시연된 것이
  아니다.
- **조치하기 전에 실행 중인 인스턴스에서 확인받고 싶은 결함 둘.** 인증기 병합 증폭 — 상수 인자에
  대해 추론했을 뿐 측정하지 않았고, 실질 심각도는 배포가 `Load()`로 키를 몇 개나 적재하느냐에
  달려 있다. 그리고 배치 모델 허용목록 우회: 핸들러와 스케줄러의 디스패치와 호출 그래프는
  읽었지만 `internal/batch/validate.go`의 행 검증 경로를 남김없이 읽지는 않았으므로 내가 놓친
  검사가 있을 가능성이 어느 정도 있다. 그 결함의 예산 쪽 절반은 확실하다 —
  `internal/batch/*.go`, `internal/app/batch.go`, `internal/app/batchstore.go`에 걸쳐
  "udget"을 `grep`하면 아무것도 나오지 않는다.
- **`internal/auth/oauth.go`(870줄)와 `internal/auth/tokenstore.go`(554줄)는 감사가 아니라 표본
  점검이다.** 내가 본 곳에서는 가림 규율이 지켜진다: `scrub`/`scrubLocked`
  (`oauth.go:651-671`)는 갱신 경로에 적용되고, `execStore.Load`(`tokenstore.go:476-501`)는
  명령 출력을 버리고 종료 상태만으로 에러를 만들며, 시크릿을 보유하는 모든 타입은 모든 `fmt`
  동사 아래에서 스스로를 가린다. "크리덴셜이 쥐었던 모든 토큰은 기록되는 무엇에서든 스크럽된다"는
  §11.2b의 주장을 모든 기록 지점에 대해 검증하지 않았고, 원자적 쓰기와 싱글플라이트의 정확성도
  검증하지 않았다. 그 서브시스템에 호출자가 없으므로(위), 이것은 그렇지 않았을 경우보다 우선순위가
  낮다.
- **`internal/cluster`는 SQL 구성에 대해서만 읽었다.** 분산 락, 리더 선출, 초과 의미론은 검토하지
  않았다. DESIGN §5.6은 `block × (nodes−1)`의 초과 한계를 공표한다. 프로세스 안에서는 맞고 노드를
  가로질러서는 틀린 예산 원장은 여기서 잡히지 않았을 것이다.
- **Lua 확장 표면(§11.5)과 이메일 경로는 검토하지 않았다.** 설정 검증이 명령어 수, 메모리, 벽시계
  상한을 요구하고(`internal/config/validate.go:740-746`) 그것은 좋은 신호이지만, 샌드박스는
  스쳐 보는 것이 아니라 자기 몫의 검토를 필요로 한다. 그 패키지는 현재 임포터가 없는 뼈대다.
- **wire 어댑터 퍼징은 시도하지 않았다.** `internal/canonical/gatediff_test.go`와
  `internal/server/fuzz_test.go`는 정확히 그 게이트/어댑터 발산 부류를 겨냥하는 것으로 보인다.
  그것들이 행사하는 프로덕션 코드는 읽었지만 실행하거나 코퍼스를 늘리지는 않았다. 수동 케이스
  분석으로는 게이트와 어댑터가 중복 키, `null` 값, 이스케이프된 키, 유효하지 않은 UTF-8에 대해
  `model`과 `stream`에서 일치하고, 내가 구성할 수 있었던 유일한 발산 — 최상위 객체 뒤의 후행
  내용 — 은 양쪽 모두 거부한다. 수동 분석만으로 그 부류를 종결됐다고 부르지는 않겠다.

### 점검했으나 결함이 나오지 않은 영역

안심시키려는 것이 아니라 커버리지 주장이 반증 가능하도록 적는다.

- **SQL 주입: 없고, 그 보증은 구조적이다.** `internal/store`와 `internal/cluster`의 모든 질의는
  `Store.exec/query/queryRow/txExec`(`internal/store/store.go:265-298`)나
  `cluster.conn/tx.exec/query/queryRow`(`internal/cluster/sql.go:33-61`)를 통과하고, 그것들은
  값을 `args ...any`로 넘기며 자리표시자를 방언별로 다시 쓴다. 그 두 패키지 밖의 어떤 패키지도
  `database/sql`을 건드리지 않는다. 동적 식별자는 롤업 테이블과 파티션에만 나타나고 닫힌
  하드코딩 리터럴 집합에서 온다. 문제에 가장 가까운 것은 트리에서 유일하게 SQL에
  `fmt.Sprintf`하는 `internal/store/partition.go:202-207`인데, 그 두 식별자 인자는 세 호출처가
  리터럴을 넘기기 때문에 오늘만 안전하고, 이웃한 `partition.go:236`과 달리 미래의 회귀를 잡을
  허용목록 단언을 달고 있지 않다.
- **메트릭 카디널리티.** `internal/server/metrics.go`, `internal/admin/metrics.go`,
  `internal/shadow/metrics.go`는 고정 카디널리티 `atomic.Uint64` 필드다. 트리에 Prometheus
  클라이언트 라이브러리가 없으므로 `WithLabelValues(callerString)` 패턴은 존재하지 않는다.

  ⚠️ **이 항목은 "어디에도 호출자가 제어하는 라벨이 없다"로 적혀 있었고, 파일 셋을 세었는데 넷이었다.**
  `internal/metrics`는 라벨이 붙는 패키지이고, 그 `model` 라벨은 요청 본문(또는 배포 경로 세그먼트)에서
  읽혀 **거부된 요청까지 포함해** 그대로 계측됐다 — 정확히 한 모델만 허용된 키도 허용되지 않은 이름을
  물어보는 것만으로 라벨 값을 찍어낼 수 있었다는 뜻이다. 시리즈 상한이 메모리와 스크레이프 크기는
  묶었지만 **해상도는 묶지 못했다**: 엔트리는 축출되지 않으므로 조작된 이름 128개가 모델별 테이블을
  프로세스 수명 내내 소진시켰고, 그 뒤 처음 관측된 모든 모델은 — 설정 reload가 방금 추가한 진짜 모델까지
  — `__overflow__`로 접혀 재시작 전까지 자기 duration·TTFT·prefix-hit 시리즈를 잃었다. 원격에서
  발동 가능하고 스스로 낫지 않는, 오퍼레이터 대시보드에 대한 해상도 서비스 거부였다.

  이 항목이 진술했어야 하고 이제 진술하는 규칙: **라벨 값은 설정에서 오며, 메시지에서 오지 않는다.**
  요청 본문에서도, 응답 본문에서도, URL 경로 세그먼트에서도, 업스트림 에러 문자열에서도 아니다.
  닫힘: `metrics.Requests.SetAdmittedModels`가 라벨 값으로 그대로 나갈 수 있는 이름을 설정된 모델
  집합으로 제한하고, 그 밖의 이름은 전부 `__unknown__` 하나로 접힌다. 물어본 이름 자체는 원장에 전체
  해상도로 남는다(`/spend/logs`). 가드는 `TestTheConfiguredSetBoundsTheModelLabel`,
  `TestAModelAddedAfterAFloodStillGetsItsOwnSeries`, `TestModelLabelAdmissionFollowsAReload`이고,
  운영 쪽 서술은 [OPERATIONS.ko.md](OPERATIONS.ko.md) §6이다.

  이것이 부재 주장의 전형적인 실패 방식이다. 이 항목은 안심시키는 문장처럼 읽히지만 실은 **논증**이었고,
  세지 않은 패키지가 하나 있었다는 이유만으로 조용히 거짓이 됐다.
- **계측 큐는 백프레셔가 될 수 없다.** `internal/meter/ring.go:83-110`과
  `internal/meter/spool.go:55-69,372-402`는 고정 크기이고, 블로킹이 아니라 드롭으로 실패한다.
  드롭은 계수되고 `metering_degraded` 플래그를 세운다.
- **prefix 테이블의 바이트 예산은 지켜진다.** `internal/prefix/table.go:125-188`은
  `MaxBytes`(기본 64 MiB)를 넘으면 동기적으로 축출한다. 고유 접두사를 쏟아붓는 호출자는 캐시를
  휘젓고 축출 CPU를 태우지만 메모리를 키울 수는 없다. (그 *키잉*은 위의 별개 HIGH 결함이다.)
- **배치 오브젝트 소유권은 올바르다.** id는 `crypto/rand` 96비트이고
  (`internal/batch/service.go:380-391`), `Retrieve`, `Cancel`, `List`, `ListFiles`,
  `FileContent`, `DeleteFile` 전부가 요청 파라미터가 아니라 인증된 주체에서 취한
  `principalID(rq)`로 필터한다(`internal/app/batch.go:344-486`). IDOR 없음. CRITICAL 결함의
  우회는 *배치가 어떤 모델을 지명할 수 있는가*에 대한 것이지 *누구의 배치를 호출자가 읽을 수
  있는가*가 아니다.
- **디스패치 경로에서 클라이언트의 크리덴셜은 프로바이더에 결코 도달하지 않는다.** `attempt`가
  새 `http.Request`를 만들고 클라이언트 헤더를 하나도 복사하지 않기 때문이다
  (`internal/app/dispatch.go:262-276`). 패스스루 경로는 헤더를 복사하되 받아들여지는 여섯 이름을
  양방향에서 전부 스트립한다(`internal/server/passthrough.go:396-397`, `:422`).
- **shadow 비교기는 조심스럽다.** 거부목록이 아니라 전달 헤더 허용목록
  (`internal/shadow/reference.go:39-47`), 그 뒤에 이중 안전장치로 `stripAuth`, 루프 가드 헤더,
  그리고 헤더의 존재는 기록하되 헤더 값은 결코 기록하지 않는 diff 리포트
  (`internal/shadow/diff.go:76-81`).
- **`peekRequest`의 W10 하드닝은 수동 분석이 닿는 한 건전하다.** 이스케이프된 키 폴백은 모델을
  찾지 못했을 때만이 아니라 이스케이프된 키를 본 적이 있으면 언제나 돌고, 태그된 구조체가 아니라
  맵으로 디코드한다 — 둘 다 옳은 선택이고 둘 다 주석이 설명한다. 거기서 내가 찾은 유일한 결함은
  성능 결함이다(위의 MEDIUM).
- **패스스루의 경로 순회 방어는 지켜진다.** 검증이 디코드된 경로에서 돌아 규칙 하나가 두 철자를
  모두 덮고(`internal/server/passthrough.go:322-344`), 결합된 경로는 정규화 후 베이스에 대해 다시
  검사되며(`:296`), `/anthropicX`는 `/anthropic` 접두사로 서빙될 수 없고(`:280-283`),
  hop-by-hop 스트리핑은 고정된 목록이 아니라 `Connection` 헤더 자신의 목록을 따른다
  (`:175-186`). 내가 시도한 어떤 인코딩으로도 순회나 매핑되지 않은 접두사 도달을 구성할 수
  없었다.

---

# 처리

위 결함들에 대해 실제로 조치한 엔지니어가 쓴다. 결함 본문 자체는 손대지 않았다. 이
절은 무엇을 했고, 무엇을 하지 않았고, 검토가 무엇을 놓쳤는지를 기록한다.

기준선: 커밋 `207d622`, 수정이 작성된 브랜치.

> **이 절은 한동안 `main`에 대해 거짓이었다.** 문서가 코드 없이 `main`으로
> 복사됐다 — 문서 끝의 "# 처리, 3차"를 보라. 그쪽이 권위 있는 서술이다. 아래의
> 모든 주장은 그 이후 **머지된 트리에 대해 재검증**됐다. 수정을 제자리에서
> 되돌리고 지명된 테스트가 깨지는지 보는 방식이다. 머지가 수정이 사는 위치나
> 하는 일을 바꾼 곳에서는, 아래 본문을 쓰인 그대로 두지 않고 바이너리에 맞게
> 정정했다. 3차가 되돌리기-실패 표 전체와 1·2차가 틀린 것들의 목록을 나른다.

양방향으로 통과하는 테스트는 개수에 세지 않고 그렇다고 기록한다.

## 통일된 진단, 적용

검토의 헤드라인 — *존재하지만 도달되지 않는 통제* — 을 명세로 취급했다. 아홉 건 중
세 건은 호출 지점을 추가하는 것이 아니라 그 누락을 표현할 수 없게 만들어 종결했다:

- **`server.Route.ModelAuth`는 필수 필드다.** 그 zero value인 `ModelAuthUnset`은
  `newRouteTable`이 거부하고, 본문 없는 라우트에 붙은 `ModelAuthGate`도 마찬가지로
  거부한다 (패스스루가 아무것도 강제하지 않으면서 인가된 것처럼 보이게 했던 바로 그
  모양이다). 라우트는 답 없이 mux에 도달할 수 없고,
  `TestEveryRouteDeclaresAModelAuthMode`는 픽스처가 아니라 실제 라우트 집합 — 기본
  라우트, 배치 라우트, 컴파일된 패스스루 접두사 — 을 훑는다.
- **`ModelAuthHandler`는 사후조건을 나른다.** `Server.serve`는 자기 모델을
  지명하겠다고 선언해 놓고 하지 않은 핸들러의 요청을 실패시킨다. 그 누락은 이제
  조용한 디스패치가 아니라 `model_auth_missing`과 로그 한 줄을 동반한 500이다.
- **`execIdentity`는 허용목록 판정 없이는 만들 수 없다.** 배치 실행기는 업스트림
  호출에 도달하려면 그것이 필요하고, `ownerResolver.identity`는 소유자가 써서는 안
  되는 모델에 대해서는 그것을 돌려주기를 거부한다. 호출은 일어나는데 검사는
  일어나지 않는 순서란 존재하지 않는다.

두 건은 더 약한 의미에서 *구조적*이 됐다 — 타입에 그 결함이 살 수 있는 필드가 더는
없다:

- `batch.UploadFile`은 `ModelAuthorizer`를 싣지 않은 `PurposeBatch` 업로드를
  거부한다. nil authorizer는 허용적 기본값이 아니라 400이다.
- `Credential`/`RotationKey`는 인라인 리터럴이 착지할 필드가 없는 모양을 거쳐
  마샬링되므로, `MarshalYAML`이 하나를 빠뜨려서 새는 일이 있을 수 없다.

나머지는 정직한 패치이며, 아래에 그렇게 표시했다.

## 결함별

상태는 머지 시점 기준이다. "종결"은 머지된 트리에서 수정을 되돌렸을 때 지명된
테스트가 깨진다는 뜻이다 — 3차가 각각에 대한 정확한 편집을 나열한다.

| # | 결함 | 상태 | 종류 |
|---|---|---|---|
| 1 | [CRITICAL] 배치가 모델 허용목록과 예산 게이트를 우회한다 | **종결**, 검증됨 | 구조적 |
| 2 | [HIGH] `rpm_limit`, `tpm_limit`, `max_parallel_requests`가 아무것도 강제하지 않는다 | **종결**, 검증됨 — 그리고 머지 전까지 `main`에서는 종결되지 **않았다**. 팀별 절반은 살아있는 결함이었다, 3차 참조 | 패치 + 연결 |
| 3 | [HIGH] 업스트림 에러 메시지가 클라이언트 봉투로 복사된다 | **종결**, 검증됨 | 구조적 |
| 4 | [HIGH] 팀 예산이 키별로 강제된다 | **종결**, 검증됨 | 패치 |
| 5 | [HIGH] 캐시 어피니티의 테넌트 격리가 출하 상태에서는 거짓이다 | **종결**, 검증됨 | 패치 |
| 6 | [HIGH] 인증되지 않은 키 폭주가 요청당 O(keys) 복사를 유발한다 | **종결**, 검증됨 — 앞 절반만. 저장소 조회 쪽 절반은 미해결 | 패치 |
| 7 | [HIGH] 비스트리밍 업스트림 읽기가 무제한 `io.ReadAll`이다 | **종결**, 검증됨 — `internal/backend`에서. 그리고 테스트는 **읽는 양** 자체를 제한하도록 강화됐다 | 패치 |
| 8 | [HIGH] 패스스루가 모델 검사 없이 디스패치한다 | **종결**, 검증됨 | 구조적 |
| 9 | [MEDIUM] `SecretRef.MarshalYAML`이 한 번도 실행되지 않는다 | **종결**, 검증됨 — 결함이 지명한 그 메서드가 아니라 `Credential`/`RotationKey`로 | 구조적 |
| — | [MEDIUM] 전송 에러 텍스트가 내부 호스트를 지명한다 | **종결**, 검증됨 | 패치 (묶음) |
| — | [MEDIUM] `x-dorang-native-error-type`이 검증되지 않고 제한되지 않는다 | **종결**, 검증됨 | 패치 (묶음) |
| — | [MEDIUM] 패스스루가 업스트림 `Set-Cookie`를 중계한다 | **종결**, 검증됨 | 패치 (묶음) |
| — | 관리 컨트롤 플레인이 마운트되지 않는다 | **수정됨** — 잠재 결함 셋을 먼저 닫고 마운트됨 | 3차 참조 |
| — | [MEDIUM] `batch.ownedBy`가 소유자 없는 레코드를 공개로 취급한다 (수정 중 발견) | **종결**, 검증됨 | 패치 |

### 1 — CRITICAL, 배치

독립적인 강제 지점 둘, 그중 하나는 디스패치되는 모든 행이 반드시 통과한다.

- **업로드 시점**, 동기적: `batch.UploadRequest.Authorize`가 `validateConfig`로
  엮이고 `validateRow`가 행마다 참조한다. 자체 코드 `model_not_allowed`를 갖는다
  (`model_not_found`와 구분되므로, 거부가 호출자에게 어떤 모델이 존재하는지 알려주지
  않는다). `internal/app`이 `server.Request.AuthorizeModel`을 감싼 클로저를
  공급하므로, 배치 행과 대화형 요청이 같은 구현에 도달한다.
- **디스패치 시점**, 행마다: `batchExecutor.Execute`가 배치의 `OwnerKeyID`를
  `internal/auth`가 쓰는 것과 같은 `recordFromAPIKey` 변환을 통해 `*auth.Principal`로
  되돌리고, 허용목록 *과* `Authorize`를 검사하고 (그래서 차단되거나 만료된 크리덴셜이
  실행 중인 배치를 멈춘다) *또* 예산 hold을 잡는다. 그 해석 결과는 30초 동안
  캐시되며 소유자 1024개로 상한이 걸린다.

예산 쪽 절반은 대화형 경로가 쓰는 것과 같은 게이트다. `budgetGate.reserveFor`가 이제
양쪽 모두에서 도달되며, 거부는 종단으로 감싸지므로 `internal/batch`가 소진된 상한을
자신의 백오프로 재시도하지 않는다.

테스트: `internal/batch/modelauth_test.go`,
`internal/app/security_test.go` (`TestBatchExecutorRefusesAModelTheOwnerMayNotUse`,
`TestBatchExecutorRefusesABlockedOwner`, `TestBatchExecutionIsBudgeted`).
수정 전: 1 USD 예산에 대해 200개 행이 업스트림 호출 200회와 함께 실행됐고 거부는
없었다.

**이것으로 닫히지 않는 것:** 하나의 허용목록 아래에서 이미 검증된 배치가, 그 뒤 키의
`models` 컬럼이 *넓어진* 다음에 디스패치되면 넓어진 목록을 쓴다 — 실행기는 현재
한도를 읽으며, 이는 의도된 설계다. 좁히는 쪽은 강제된다. 중요한 방향은 그쪽이다.

### 2 — HIGH, 레이트·동시성 상한

`auth.Access`에 `Rates auth.RateSource`가 생겼고, `Limits.authorize`는 이제 주체의
**id**를 받으므로 팀 상한이 팀의 카운터와 비교된다. `ObservedRPM`/`ObservedTPM` 한
쌍은 단일 주체용 폴백으로 남아 있고, 기존 단위 테스트가 여전히 건드리는 것이 바로
그것이다 — 그 테스트는 수정 전에도 후에도 통과하며 아무것도 종결하지 않는다. 검토가
지명한 바로 그 위상이다. 이것을 종결하는 테스트들은 `app.principal.Authorize`를 지나는
종단간 테스트다.

`internal/app/rates.go`가 카운터다. 머지된 형태는 **롤링** 1분이다 — 주체당 타임스탬프가
찍힌 1초 버킷 60개, 16-way 샤딩, 주체 100 000개 상한에 오래된 엔트리 축출 — 키는
`kind:id`이므로 요청마다 세 주체가 모두 계수되고, 서로 다른 주체 id의 폭주는 단순히
리셋되는 것이 아니라 제한된다. (브랜치 쪽 버전은 텀블링 1분이었고 롤오버 때 맵을 통째로
버렸다. `main`은 키별 일제 점검을 위해 롤링 쪽을 독립적으로 작성해 두었고, 머지는 롤링
윈도를 유지하면서 그것을 주체별로 재도출했다. 이유는 3차가 기록한다.) 토큰은 정산
시점에 `App.recordMetrics`에서 더해지며, 이곳이 끝난 요청이 모두 지나는 유일한 프로덕션
지점이다.

`max_parallel_requests`는 `capacity.Request.PrincipalMax`를 통해 브로커에 도달하며,
정적 `capacity.principals` 테이블과는 더 제한적인 쪽을 취하는 방식으로 결합된다 —
키별 상한은 배포 기본값을 조일 수는 있어도 절대 올릴 수 없다.

테스트: `TestRPMLimitIsEnforced`, `TestTPMLimitIsEnforced`,
`TestTeamRPMCountsEveryKeyUnderTheTeam`, `TestRateWindowRolls`,
`TestSettledTokensReachEverySubjectOfTheRequest`,
`TestRPMLimitRefusesThroughTheWholeStack`, `TestTPMLimitCountsFinishedTokens`
(둘 다 조립된 스택을 통과하며 실제 업스트림을 상대로 돈다),
`TestMaxParallelReachesThePrincipal`, 그리고 `internal/capacity/queue_limits_test.go`의
`TestPrincipalMaxTightensTheAxis` / `TestPrincipalMaxNeverWidens`.

**완화됐을 뿐 종결되지 않음:** `tpm_limit`은 *다음* 요청을 제한한다. 토큰 수는 정산
전에는 존재하지 않기 때문이다. 거대한 요청 하나는 상한을 한 번 넘을 수 있다. 레이트
카운터는 또한 프로세스별이다. 다중 노드 배포는 상한의 N배를 강제한다. 영속 원장이
존재하며 그것이 이를 닫을 수 있지만 요청당 저장소 쓰기가 대가다 — 채택하지 않았다.

### 3 — HIGH, §11.3

`Normalize`에는 어느 분기에도 디코드된 본문에서 `Error.Message`로의 대입이 없다.
업스트림의 텍스트는 512바이트로 제한된 `Error.NativeMessage`로 가고, 본문에는 상태
코드만을 입력으로 받는 함수인 `canonicalMessage(status)`가 들어간다. 새 분기는 규칙을
잊어서 누출할 수 없다. 누출을 직접 작성해야만 한다.

스크러버는 `internal/backend`에서 돈다. L5 추출 이후 실제로 HTTP 호출을 하는 계층이다.
`upstreamError`는 나가는 크리덴셜 헤더에 실제로 얹힌 것을 수집하고 —
`collectSecrets`가 크리덴셜 테이블이 아니라 헤더를 읽으므로, 자기가 소유하지 않은
코드가 붙인 OAuth 토큰도 함께 잡힌다 — 무엇이 렌더링되거나 기록되기 전에
`NativeMessage`, 네이티브 타입, 코드에서 그것을 지워 낸다. 그래서 원장 사본도
스크럽된다. `internal/redact`가 공유 구현으로 존재한다. `internal/app`은 더는 그것을
적용하지 않는데, `internal/app`이 더는 업스트림 응답을 건드리지 않기 때문이다.
`internal/probe`가 가진 자체 `scrubber`는 세 번째 사본이며 그대로 두었다. 그 작업이
착수되면 `internal/redact`로 갈아타야 한다.

묶여 있던 MEDIUM들도 함께 갔다. `WriteError`는 업스트림이 통제하는 `NativeType`에
`safeHeaderValue`와 128바이트 클램프를 적용하고, `transportError`는 `err.Error()`를
중계하는 대신 전송 실패를 로그에 남긴다 — dial 실패의 경우 그 문자열은 운영자의 내부
호스트, 포트, 해석된 주소를 지명한다.

테스트: `internal/server/errorleak_test.go` (봉투 모양 일곱 가지 전부),
`TestAHostileUpstreamCannotEchoTheCredentialBack` (종단간, 자신이 건네받은
`Authorization` 헤더로 답하는 백엔드),
`TestUnreachableUpstreamDoesNotNameInternalHosts`,
`TestNativeErrorTypeHeaderIsCheckedAndClamped`.

**머지가 찾아낸 파급:** `Error.Message`가 이제 모든 분기에서
`canonicalMessage(status)`이므로, 업스트림의 단어로 *분류*하던 것은 무엇이든 그것을
따라 `NativeMessage`로 옮겨갔어야 했다. `internal/app/estimate.go`의 `upstreamCause`는
그러지 않았고, §10.5a의 "더 큰 윈도로 라우팅"은 업스트림 신호로부터 도달 가능하기를
조용히 그만둔 상태였다. `TestAnUpstreamOverflowIsRecognisedFromTheBody`가 이를
고정한다.

**의도적으로 하지 않은 것:** 검토는 디버깅 편의를 위해 발췌를 중계하는 운영자 플래그를
제안했다. 추가하지 않았다 — 유일한 효과가 취약점을 다시 켜는 것뿐인 플래그이고,
네이티브 텍스트는 원장과 로그에 있다.

### 4 — HIGH, 예산 주체

`budgetSubjectsOf`는 한도를 선언하는 주체마다 하나씩, 각자의 한도 *와 각자의 주기*를
갖는 주체를 돌려주고, `reserveFor`는 주체마다 hold을 하나씩 잡되 뒤쪽의 하나가 거부하면
이미 잡은 것을 전부 푼다. 부차 결함 — 최소 한도가 첫 번째 비어 있지 않은 주기와
짝지어지던 것 — 도 같은 변경으로 닫히며, 자체 테스트를 갖는다.

테스트: `TestTeamBudgetIsNotMultipliedByTheNumberOfKeys` (하나의 1 USD 팀 상한 아래
10개 키에 걸친 요청 100건. 수정 전에는 하나도 거부되지 않았다),
`TestEachBudgetSubjectKeepsItsOwnPeriod`.

### 5 — HIGH, 테넌트 스코핑

`router.Request.Tenant`는 프로덕션에서 `tenantOf(rq.Principal)`로부터 대입된다. 팀,
없으면 유저, 없으면 키이며, 각각에 접두사가 붙어 팀 id와 유저 id가 충돌할 수 없다.
`prefix.NewChain`/`Compute`는 테넌트를 받아 `h₀ = H(tenant ‖ 0x00 ‖ group)`으로
시드한다. 구분자에는 테스트가 있는데, 그것이 없으면 한 테넌트가 다른 테넌트와 같게
시드되는 그룹 이름을 고를 수 있기 때문이다.

`internal/batch`의 `groupHash`는 의도적으로 빈 테넌트를 넘긴다 — 그 다이제스트는 한
배치 자신의 행들을 정렬할 뿐 라우팅 테이블에 닿지 않는다.

이것은 **DESIGN §7.4b에 쓰인 것으로부터의 이탈**이다 (`h₀ = H(group_id)`). 설계 쪽이
결함이라는 검토의 지적이 옳다. §7.4b를 개정해야 한다.

테스트: `internal/prefix/tenant_test.go`, `TestRouterRequestCarriesTheTenant`.

### 6 — HIGH, 인증기 폭주

`insert`는 승격 가능한 엔트리를 세고 그것들에 대해서만 병합한다. 네거티브는 자체
상한(`maxNegative = 1024`)을 갖고 단독으로 버려지므로, 폭주는 공격자 자신의 캐시를
비용으로 치를 뿐 그 옆에서 학습된 정당한 크리덴셜을 비용으로 치르지 않는다.

새 테스트로 수정 전 코드에서 측정: **미지의 키 2000개당 스냅샷 병합 1937회**로,
검토의 산술과 일치한다. 수정 후: 한 자릿수.

테스트: `internal/auth/flood_test.go`.

**종결되지 않음:** 검토의 두 번째 권고 — 미지의 키에 대한 *저장소 조회*를 출처별로
레이트 제한하거나 합치라 — 는 구현하지 않았다. 인증되지 않은 요청당 `LoadByLookup`
한 번이 그대로 남아 있고, SQLite를 상대로는 그것 자체가 하나의 증폭 계수다. 네거티브
TTL은 같은 키의 반복을 제한하고, 서로 다른 키는 아무것도 제한하지 않는다. **이번
차수에서 미해결로 남은 것 중 가장 큰 항목이다.**

### 7 — HIGH, 무제한 읽기

`backend.readUpstreamBody`는 `Options.MaxResponseBytes`(기본값
`backend.DefaultMaxResponseBytes`, 32 MiB, 요청 상한과 일치)보다 1바이트 더 읽고
`502 upstream_response_too_large`로 답한다. 재시도 불가다 — 폴백 홉이 또 하나를
버퍼링할 것이기 때문이다. 이것이 `internal/backend`에 있는 이유는 그 패키지가 호출을
하는 쪽이기 때문이다. 배치 실행기도 같은 `backend.Do`를 통해 여기 도달하므로, 검토가
언급하지 않은 두 번째 무제한 읽기는 두 번째 사본이 아니라 같은 코드로 종결된다.

`MaxResponseBytes`는 백엔드의 옵션이지만 **아직 YAML에 노출되지 않았다**.
`server.max_body_bytes`도 같은 구멍이 있었으나 지금은 아니며(CONFIG §2), 그래서 이것만
홀로 남았다.

테스트: `TestOversizedUpstreamResponseIsRefused` — 이제 리더에게 **요구된** 양을 센다.
그 첫 버전은 본문 전체를 읽고 나서 측정하는 구현에 대해서도 통과했기 때문이다.

### 8 — HIGH, 패스스루

검토가 선호한 (a)가 아니라 (b), 제한된 peek을 택했다. 시작 시점 거부는 계산될 수 없기
때문이다. 키는 데이터베이스에 살고 그 `models` 컬럼은 런타임에 바뀌므로, "설정된 어떤
키도 허용목록을 나르지 않는다"는 `compilePassthrough`가 알 수 있는 사실이 아니다.

`authorizePassthroughModel`은 최대 1 MiB를 읽고, `io.MultiReader`로 그 바이트를
릴레이에 그대로 되돌려주며(전달되는 요청은 바이트 단위로 동일하다 — 테스트가 있다),
찾아낸 것에 대해 `Request.AuthorizeModel`을 호출한다. 모델을 확정할 수 없을 때 —
JSON이 아닌 본문, peek 창을 넘어가는 본문, 객체가 아닌 JSON — 답은 principal에서
나온다: **모델을 하나라도 제한하면 거부, 아무것도 제한하지 않으면 허용.** 제한된 키에
대해서는 WebSocket 업그레이드가 즉시 거부된다. 프레임은 절대 디코드되지 않기
때문이다.

`server.ModelRestricted`는 "목록이 있는가"에 답하는 선택적 인터페이스다. 이를 구현하지
않은 `Principal`은 제한된 것으로 취급된다. fail-closed이므로, 메서드 하나를 잊었을
뿐인 미래의 구현은 강제가 아니라 가용성을 대가로 치른다.

두 거부 모두 COMPATIBILITY §11.2에 따라 **403 `permission_error`**다 — 크리덴셜은
인증됐고 단지 이 모델이 허용되지 않을 뿐이며, 401은 클라이언트에게 사실은 다시
인코딩해야 할 본문을 두고 재인증하라고 말하는 것이다. 브랜치는 §7.2를 인용해 401로
답했는데, 그 절은 태그 라우팅 실패에 관한 것이다. 공유 인가 게이트는 같은 거부에 대해
언제나 403으로 답해 왔으므로, 하나의 규칙에 두 개의 답이 있었고 그중 틀린 것은 도달할
수 없는 쪽뿐이었다.

테스트: `internal/server/passthrough_security_test.go`.

**완화됐을 뿐 종결되지 않음:** 정당한 비-JSON 패스스루 본문(오디오 업로드, 이미지
편집)을 가진 제한된 키는 이제 거부된다. 이전에는 허용목록을 통째로 우회하던 경로에서의
기능 회귀이고 fail-closed 방향이기는 하지만, 운영자는 이를 알아챌 것이다.

### 9 — MEDIUM, `dorangctl import config`

변경 두 가지. 그리고 결함 자체의 제목이 잘못된 메서드를 지명하고 있다.
`SecretRef.MarshalYAML`은 **한 번도 실행되지 않는다** — 그것이 결함이다 — 그리고 지금도
여전히 실행되지 않는다. 이름을 바꿔 존재하지 않게 만들어도 모든 마샬링 테스트가
통과한다. 수정은 한 겹 바깥에 있다. `Credential`과 `RotationKey`는 명시적 `MarshalYAML`
메서드를 갖고, 그 대상 모양에는 인라인 리터럴이 차지할 수 있는 필드가 없으므로
`,inline` 평탄화가 거기 닿을 수 없다. 그리고 `internal/config/import.go`는 더는 인라인
리터럴을 *만들지* 않는다. 소스의 리터럴 `api_key`는
`key_env: DORANG_<PROVIDER>_API_KEY`와 그 변수를 지명하는 경고가 된다. 비밀은 경고에도
그대로 찍히지 않는다 — "비밀이 있어서는 안 될 곳에 갔다"의 수정은 그것을 다른 곳에
출력하는 것이 아니다.

테스트: `internal/config/secretmarshal_test.go`,
`TestImportWarnsAboutALiteralKey` (리터럴이 없음을 단언하도록 다시 썼다).

**운영자에게 오는 동작 변경:** `dorangctl import config`의 출력은 이제 운영자가 지명된
환경변수를 먼저 설정하지 않으면 서버를 띄우지 못한다. 의도된 것이다.

## `internal/admin` — 마운트됨

**이 절은 "연결되지 않은 채로 둠"이라고 적혀 있었고, 그것은 더는 사실이 아니다.**
마운트됐다. 잠재 결함 셋을 먼저 닫고 나서다 — 인증 이전의 읽기, `api_keys.team_id`에서
도출되는 팀 범위 관리자, 그리고 신뢰되는 대신 검사되는 요청 제공 id 필터. 연기했던
논거는 쓰인 그대로 유효하다. 그 셋 없이 연결했다면 잠재 결함 셋이 커밋 하나로 살아있는
결함으로 바뀌었을 것이다. 그것들을 고쳤고, 그다음 연결했다. 상세와 테스트는 3차에
있다.

### 유출된 키 폐기

`POST /key/block`이 서빙한다. `TestALeakedKeyCanBeRevokedThroughTheAPI`
(`cmd/dorang/revoke_test.go`)는 그 사건을 HTTP 위에서 종단간으로 실행한다. 동작하는
키, 관리 호출 한 번, 그리고 그 키로부터의 다음 요청이 거부되며, 감사 행을
데이터베이스에서 다시 읽어 확인한다. 거부는 `DefaultEntryTTL`(60초) 안에 관측된다.
`internal/auth`가 로드된 크리덴셜을 캐시하기 때문이다 — 테스트는 즉시 단언하는 대신
그것을 기다린다. 운영자의 질문은 "동작이 멈추기까지 얼마나 걸리는가"이고 그 답은
"영영 안 멈춘다"가 아니라 제한된 숫자여야 하기 때문이다.

`POST /key/regenerate`는 비밀을 즉시 교체하고, `POST /key/rotate`는 유예 창을 두고
교체한다(§11.2c). 둘 다 `store.RotateKey`를 거치며, regenerate는 그 뒤에 `EndGrace`를
붙인다. 두 번째 쓰기 경로는 없다. 존재했던 그 경로는 `api_keys`의 비정규화된 verifier
컬럼에 썼는데 인증은 `api_key_secrets`를 통해 해석되기 때문이다 — 200 뒤에 숨은,
아무것도 재생성하지 않는 재생성이었다.

SQL 비상 경로는 여전히 동작하고, 한 경우에는 여전히 옳은 답이다. 관리 크리덴셜 자체가
유출된 경우다.

```sql
UPDATE api_keys SET blocked = 1 WHERE id = '<key id>';
```

`internal/auth`는 로드된 크리덴셜을 `DefaultEntryTTL`(60초) 동안 캐시하므로 그 키는
쓰기 후 1분 이내에 동작을 멈추고, `Authenticator.use`는 `Authorize`와 독립적으로 매
요청마다 `Blocked`를 강제한다. 비밀 없이 키 id를 찾는 법: 라벨은 조회 다이제스트의 앞
16진수 8자로부터 도출되므로(`store.LabelFor`),
`SELECT id, key_label, user_id, team_id FROM api_keys WHERE key_label = ?`가 유출
신고에 보통 담기는 것만으로 그 키를 식별한다.

**진행 중인 배치:** 그 행을 차단하면 그 키가 제출한 배치들도 소유자 해석 TTL(30초)
이내에 멈춘다 — `batchExecutor`가 행마다 `Authorize`에 도달한다. 이 변경 이전에는 차단
이전에 제출된 배치가 끝날 때까지 계속 지출했다.

## 검토가 놓친 것

수정 중에 발견됐고, 위 결함 목록에는 없다.

1. **`.gitignore`가 `cmd/` 트리 전체를 추적에서 뺀다.** `dorang`과 `dorangctl` 패턴에
   앵커가 없어서, 빌드된 바이너리뿐 아니라 *디렉터리* `cmd/dorang`과
   `cmd/dorangctl`에도 매치된다. `cmd/dorang/main.go` — 게이트웨이의 진입점 — 는 한
   번도 버전 관리에 들어간 적이 없다. `git status`는 깨끗한 트리를 보여주고, 새로
   클론하면 빌드되지 않는다. 두 패턴을 리포지토리 루트에 앵커링해서 수정했다. 검토가
   `git ls-files`에 나오지 않는 파일을 두고 `cmd/dorang/main.go:173`을 인용할 수
   있었던 이유가 이것이다.

2. **배치 실행기에도 결함 7과 같은 무제한 `io.ReadAll`이 있었다**
   (`internal/app/batch.go`, 수정 전 266행). 검토는 대화형 쪽만 찾았다. 이제 둘 다
   `readUpstreamBody`를 쓴다.

3. **`batch.ownedBy`가 소유자 없는 레코드를 공개로 취급했다.**
   `ownedBy(recordOwner, owner)`는 `recordOwner == ""`일 때 true를 돌려줬고, 그래서 빈
   `OwnerKeyID`로 생성된 파일이나 배치는 *모든* 키가 읽고, 쓰고, 삭제할 수 있었다.
   마스터 크리덴셜 업로드가 정확히 그런 레코드를 만들었다. 양쪽 절반 모두 **수정**했다.
   어느 한쪽만으로는 불완전하기 때문이다. `principalID`는 마스터 크리덴셜에 예약된,
   비어 있지 않은 소유자(`app.MasterOwnerID`)를 부여하고, `ownedBy`는 더는 빈
   `recordOwner`를 모두의 것으로 읽지 않는다. 대안 — "소유자 없는 레코드는 마스터
   크리덴셜의 것이다" — 은 기각했다. 그 대안은 **쓰기** 경로가 소유자 없는 행을 계속
   만들게 놔두고, 모든 레거시 행을 마스터 크리덴셜이 한 일로 다시 라벨링하는데, 이는
   감사의 관점에서 거짓말이기 때문이다. 잔여물은 fail-closed 방향이다. 이미 빈 소유자를
   가진 행은 이제 관리 호출자에게만 보인다.
   테스트: `TestAnUnownedRecordIsNotVisibleToEveryKey`,
   `TestTheMasterCredentialOwnsWhatItCreates`.

4. **배치 행의 본문이 그 행과 다른 모델을 지명할 수 있었다.** 스케줄러는
   `row.Model`(검증 시점에 추출)을 나르고 실행기는 `req.Body`를 독립적으로 디코드했다.
   허용목록 검사는 한쪽에 대해, 디스패치는 다른 쪽에 대해 이뤄졌을 것이다.
   `batchExecutor.Execute`는 이제 그 불일치를 거부한다. 수정 전에는 애초에 아무것도
   검사되지 않았기 때문에 악용 가능하지 않았고, 둘 중 잘못된 쪽에 검사가 추가되는
   순간 악용 가능해졌을 것이다.

5. **예산 리팩터가 들여온 잠재 nil 역참조. 그 자신의 테스트가 잡았다:** `estimate`는
   게이트의 시계를 읽으므로, 설정되지 않은 게이트는 거기 도달해서는 안 된다. 두 진입점
   모두 `g == nil || g.ledger == nil`을 먼저 검사한다. 이것을 기록하는 이유는 테스트의
   존재 근거이기 때문이다. 그 리팩터는 눈으로 두 번 검토됐고, panic은
   `TestBatchExecutorRunsAnAllowedModel`이 찾았다.

## 다루지 않은 것

검토의 결함 중 의도적으로 미해결로 남긴 것들. 다음 차수를 위한 우선순위 순이다:

- **미지의 키에 대한 저장소 조회가 제한되지 않는다** (결함 6, 뒤 절반).
  남은 것 중 가장 큰 항목.
- [MEDIUM] 요청 본문이 동시성 게이트 이전에 버퍼링되고, `max_response_bytes`가 YAML에
  노출되지 않는다. `server.max_body_bytes`는 이제 노출된다.
- [MEDIUM] `ReadTimeout`/`IdleTimeout` 없음, 본문 읽기 데드라인 없음 — slow POST.
- [MEDIUM] `/v1/files`에 키별 쿼터 없음, 파일 개수 제한 없음, 만료 없음.
- [MEDIUM] 임베딩 릴레이가 엄격한 fold 충돌 필터를 적용하지 않는다.
- [MEDIUM] 이스케이프된 최상위 키가 하나라도 있으면 핫패스에서 전체 unmarshal이
  강제된다.
- [MEDIUM] 허용되는 인증 헤더 목록이 서로 독립적으로 둘 존재한다.
- [LOW] `shadow.Options.ReferenceKey`가 마스킹되지 않음. 라우트 테이블이 인증 이전에
  열거 가능. WebSocket 릴레이가 계측되지 않고 타임아웃도 없음.

---

# "configured but never applied" 일제 점검

검증되고 로드되지만 아무것도 읽지 않던, 남아 있던 열두 개 제어 항목에 대한 별도 차수 —
DESIGN §17.1이 이 코드베이스의 지배적 결함 유형으로 지목한 그 유형이다. 여기서 위의
결함을 고쳐 쓰는 것은 없다. 앞선 처리가 틀린 것으로 드러난 곳은 아래에서 이름을 들어
정정한다.

## 먼저 해야 할 정정

**결함 2는 종결로 기록됐으나 종결이 아니었다.** 위의 표는 `rpm_limit`, `tpm_limit`,
`max_parallel_requests`를 **종결 | 패치 + 연결됨**이라 적었고, 본문은
`auth.Access.Rates auth.RateSource`, 텀블링 분 창을 쥔 `internal/app/rates.go`,
`capacity.Request.PrincipalMax`, 그리고 이름 붙은 테스트 다섯 개를 서술했다. 그중 무엇도
존재하지 않았다. `RateSource`나 `PrincipalMax`를 `grep`하면 아무것도 나오지 않았고,
`internal/app/rates.go`는 파일이 아니었으며, `TestRPMLimitIsEnforced`는 이 문서 안에만
있었다.

그것은 고쳤다고 주장한 결함보다 나쁘다. 종결된 결함은 다시 확인되지 않기 때문이다. 그리고
그것은 한 단계 위에서 벌어진 같은 실패 형태이기도 하다 — 서류상으로 충족되고 아무데도
연결되지 않은 처리. 뒤따르는 규칙은 §17.1이 코드에 대해 이미 말하는 것을 검토에 적용한
것이다 — **파일이나 테스트를 지목하는 처리는 그 파일이나 테스트가 존재할 때에만
종결된다**. 그리고 그것을 지키는 방법은 독자가 직접 실행할 수 있는 것을 인용하는 것이다.

이 제어는 이제 종결됐고, 구현은 그 처리가 서술한 것과 다르다. 우연히 일치하기를 기대하고
내버려 두는 대신 아래에 그대로 써 둔다.

## 연결한 것

| # | 제어 항목 | 현재 동작 |
|---|---|---|
| 1 | 키별 `rpm_limit` / `tpm_limit` | `internal/app/rates.go`가 롤링 1분 창을 유지한다: 초 스탬프가 찍힌 1초 버킷 60개, 16방향 샤딩, 콜드 엔트리 축출과 함께 최대 100 000개 주체. `app.principal.Authorize`가 `auth.Access`의 유일한 프로덕션 생성 지점에서 관측 카운터를 공급한다 — 그 지점은 카운터를 누락했고, 그래서 모든 양수 상한이 하드코딩된 0과 비교됐다. 요청은 게이트에서 세고(끝난 요청에 대해서만 강제되는 상한은 버스트를 거부할 수 없다) 토큰은 정산 시점에 센다. **머지에서 정정됨:** 여기 쓰인 대로는 창이 api 키 **하나만으로** 키잉됐고 `auth.Access`가 세 주체에 대해 관측 쌍 하나만 날랐다. 그래서 **팀**의 상한이 한 **키**의 카운터와 비교됐다 — 팀 한도가 그 아래 키 개수만큼 곱해진 것이다. 지금은 `kind:id`로 키잉되고 `auth.RateSource`를 통해 주체별로 읽는다. 3차를 보라 |
| 1 | 키별 `max_parallel_requests` | `capacity.Request.PrincipalMax`. 정적 `capacity.principals` 표와 **더 제한적인 쪽**을 취해 결합한다: 키 자신의 상한은 설정된 상한을 조일 수는 있어도 결코 넓힐 수는 없다. 그 값은 파일에 있지 않고 키를 편집하면 바뀌므로 요청과 함께 이동한다 |
| 2 | `capacity.*.max_queue` | `internal/capacity`의 축별 대기열 깊이 상한. 그것을 넘으면 `Acquire`가 `ErrQueueFull`을 반환하고 아무것도 큐에 남기지 않는다. 이전에는 대기 큐가 무한 힙이었다 |
| 2 | `capacity.principals.<id>.max_queue_wait` | `Acquire`의 대기 예산. 초과하면 `ErrQueueTimeout`을 반환한다. 라우터는 **핀된** 요청에 이것을 쓴다. 핀된 요청이야말로 진짜로 기다려야 하는 요청이기 때문이다 — 핀되지 않은 요청은 스필하거나 폴백하며(§7.4a2, §7.6), 그것을 대신 큐에 넣는 것은 건강한 백엔드로의 빠른 홉을 포화된 백엔드에서의 느린 대기와 맞바꾸는 짓이다. batch에는 적용되지 **않는다**: batch는 기다리라고 있는 작업이고(§11.1), 모든 principal이 지니는 30초 기본값은 평범한 경합을 실패한 행으로 바꿔 놓았을 것이다 |
| 3 | `metering_degraded` | `server.HealthReporter`이고 `app.meterHealth`가 구현한다. `GET /health`가 이제 저하 여부와 무관하게 모든 응답에 `"metering":{"degraded":…,"reason":…,"dropped":…,"spool_bytes":…}`를 싣는다 — 실패했을 때만 보고하면 운영자는 "저하되지 않음"과 "이 빌드는 그것을 보고하지 않음"을 구별할 수 없다. 상태 코드는 절대 바꾸지 않는다: 트레이스를 잃는 것은 데이터 품질 실패지 서빙 실패가 아니다 |
| 4 | `observability.prometheus` | false면 `/metrics` 라우트를 통째로 제거한다. 그래서 빈 본문으로 200을 답하는 대신, 서빙되지 않는 다른 라우트와 똑같이 501을 답한다 |
| 4 | `/metrics` 인증 | 이 라우트는 기본이 `Admin`이다: 마스터 크리덴셜, 또는 새로 추가된 선택적 `server.AdminPrincipal`을 구현한 principal. `observability.metrics.public: true`가 의도적으로 그것을 연다. 이전에는 컨테이너 프로브와 나란히 `Public`이었고, 그것은 키별 지출과 크리덴셜별 쿼터 상태와 설정된 모델 목록 전체를 인증 없는 포트에 올려놓는 것이었다 |
| 5 | `key_rotation.strategy` | `router.Rotation`. `Router.eligible`에서 선호 크리덴셜로 적용된다. `failover`가 예전 동작이고, `round_robin`은 배포별 커서를 전진시키며, `least_used`는 브로커에 묻고(`Broker.LeastUsedKey`, 풀 전체에 락 하나), `random`은 균등하게 고른다. 크리덴셜 핀과 sticky 엔트리는 둘 다 이것보다 우선한다: 그 둘은 대화에 관한 진술이고, 로테이션은 부하에 관한 진술이다 |
| 6 | `quota_urgency` | `router.StrategyQuotaUrgency`. **양쪽** 이름 목록에 추가했고, 내림차순으로 순위를 매기는 `compare` 케이스와 `quota.Ranker`가 공급하는 `Deps.Urgency`를 함께 넣었다. 스코어러도, 점유율 댐핑도, 노드별 지터도 이미 전부 있었고 호출자가 없었다 |
| 7 | `client_priority` / `range` | 클래스 이름 둘로 된 `range`와 함께 쓰는 `capacity.principals.<id>.client_priority: allow`. `router.PriorityConfig.Grants`로 컴파일되고 `CanonicalFor(principal, …)`가 적용한다. 인바운드 힌트는 `X-Request-Priority`에서 읽고, 부여되지 않은 힌트는 `x-dorang-dropped-params`에 보고된다 — §10.5가 요구하는 것이고 이전에는 침묵했다. 키의 `priority_class`도 이제 라우터에 닿는다 — 그것은 로드되고 principal에 실려 다녔지만 Lua 훅 뷰 말고는 아무것도 읽지 않았다 |
| 8 | `DORANG_STATE_DIR` | `config.ExpandPath`가 선두의 `~`를 이 값으로 해소한다. 출하되는 상태 경로는 전부 `~/.dorang/…`이므로, 이미지의 `nonroot` 사용자 아래에서는 데이터베이스와 스풀과 **생성된 키 페퍼**가 `/home/nonroot`에 쓰였다 — 선언된 볼륨 바깥이고, 재시작하면 사라진다. 페퍼를 잃으면 발급된 모든 api 키를 검증할 수 없게 된다 |
| 8 | 이미지의 `HEALTHCHECK` | `dorangctl health`가 이제 존재한다. 이미지는 그것을 호출했는데 CLI는 `unknown command "health"`를 답하며 2로 종료했다. 그래서 모든 컨테이너가 `start-period + 3 × interval` 이후 영원히 unhealthy를 보고했다 — 게다가 distroless라서 그 안의 다른 무엇도 프로브할 수 없었다 |
| 10 | 모델 허용목록 상태 코드 | COMPATIBILITY §11.2에 따라 403 `permission_error`. 그 분기는 §7.2를 인용했는데 §7.2는 **태그 라우팅**에 관한 것이다. 공유 인가 게이트는 같은 거부에 대해 언제나 403을 답해 왔으므로, 규칙 하나에 답이 둘이었고 그중 도달 불가능한 쪽만 틀렸다. 머지 이후 이 검사는 `handleInference` 안이 아니라 모든 `ModelAuthGate` 라우트의 **게이트**에서 실행된다. `handleInference`는 여섯 개 패턴으로 도달 가능했고 그 밖으로는 도달할 수 없었다 — 그래서 403은 이제 그 검사가 이전에는 닿지 못하던 라우트들을, 패스스루를 포함해 덮는다 |
| 12 | 인라인 `notional_rate` | `pricing.rules[]`에서 `source`와 `as_of`를 필수로 하여 받아들이고, 브리지를 건너 카탈로그 쪽 표기로 실어 나른다. 클래스 이름만 추가하는 것은 수정이 되지 못했을 것이다: `internal/pricing`은 출처 없는 notional 규칙을 거부하므로, 변환이 그것을 함께 날라야 했다 |
| 12 | `cached_read` / `cache_read` | 두 표기 모두 두 파일에서 받아들여지고 하나의 컴포넌트로 해소된다. 한 규칙 안에서 같은 컴포넌트가 두 이름으로 나오면 한 가지를 두 번 가격 매기는 것으로 보고 거부한다 |
| — | 백엔드별 `routing.prefix.ttl` | 목록에 없음. 아래를 보라 |

## 거부하게 만든 것

각 거부는 설정 이름과 대신 무엇을 쓸지를 알려 준다. 그것이 거부와 벽의 차이 전부다.

| 설정 | 거부 |
|---|---|
| 모든 그룹, `global`, `models[]`의 `capacity.*.rpm`, `.tpm` | "a rate ceiling is not enforced on a capacity axis… put the ceiling on the deployment instead — `models[].deployments[].limits[]` with `metric: rpm` or `tpm`" |
| `capacity.principals.<id>.rpm`, `.tpm` | 같은 것. 다만 api 키를 지목한다: `dorangctl key create --rpm N --tpm N`. 위의 항목 1이 이제 강제하는 그것이다 |
| `key_ref`가 나오는 모든 자리 | "key_ref is not resolved by this build… Use `key_env` or `key_file` — a vault agent that writes a file or exports a variable satisfies both" |
| `pricing.rules[].rates.images` | 이제 조립 에러가 아니라 **로드** 에러다. 예전에는 `dorangctl config lint`를 통과한 다음 서버가 시작되지 못하게 만들었다 |
| `source`나 `as_of`가 없는 `notional_rate` 규칙, 그리고 다른 클래스에 붙은 출처 | §8.5에 따라 양방향 모두 |
| `range` 없는 `client_priority: allow`, 부여 없는 `range`, 선언되지 않은 클래스를 지목하는 `range` | 반쯤 쓰다 만 부여는 이 일제 점검이 없애려고 존재하는 바로 그 형태다 |

### `rpm`/`tpm`을 용량 축에 연결하지 않고 거부하는 이유

과제가 물은 질문 — 어느 패키지가 옳은 집인가 — 에는 답이 하나다. 용량 축은 **동시 예약**을
세고 그것은 요청이 끝나면 해제된다. 레이트는 **한 윈도우 안의 이벤트**를 세고 그것은 결코
해제되지 않는다. 브로커에는 시계도 윈도우도 없으며, 그것을 주는 것은 `internal/quota`를
복제하는 일이다. `internal/quota`가 정확히 그것을 소유하고 있고 설정 표면도 이미 양쪽 다
갖고 있다: 크리덴셜 축에는 `models[].deployments[].limits[]`, 호출자 축에는 api 키 자신의
`rpm_limit`/`tpm_limit`. `capacity.*.rpm`을 연결했다면 다른 두 표기가 이미 강제하고 있는
상한의 **세 번째** 표기가 생겼을 것이고, 그것은 결함 유형에 대한 수정이 아니라 결함 유형
자체다.

`tpm`에는 그 축에서 두 번째의, 결정적인 문제가 있다: 입장 시점에는 토큰 수가 존재하지
않는다. 거기서 강제하려면 추정치를 청구하고 나중에 정산해야 한다 — 바로 옆 패키지
`internal/quota`가 이미 구현한 그 메커니즘이다.

## 목록에 없었으나 점검 중 발견한 것

1. **`routing.prefix.ttl`은 전역적이지 않은 것에 대한 숫자 하나였다.** 어피니티 표의 엔트리
   수명은 **백엔드**가 어떤 프리픽스의 KV 블록을 얼마나 더 쥐고 있는지를 모델링하는데,
   그것은 자릿수 이상으로 다르다: OpenAI의 자동 캐싱은 대략 5분, Anthropic은 기본 티어에서
   5분인 반면 확장 티어에서는 1시간, Gemini의 명시적 캐싱은 운영자가 정한 TTL — 그리고
   vLLM과 SGLang에서는 애초에 시간 값이 아니다. 블록이 메모리 압박 아래 LRU 축출 때까지
   살아 있기 때문이다. 1시간은 양방향으로 동시에 틀렸다: 호스티드 백엔드에는 너무 길어서,
   더 이상 프리픽스를 쥐고 있지 않은 노드에 대화를 고정시키고 아무 대가 없이 부하 분산을
   잃는다. 셀프호스티드에는 너무 짧아서, 아직 남아 있던 히트를 버린다.

   이제 프로바이더별·배포별로 설정할 수 있고 전역값을 상속하며, 시계가 없는 부류에는
   `until_evicted`가 있다. **주장은 하기 전에 확인했다**: `internal/prefix`는 두 패스로
   축출한다 — 만료된 것을 먼저, 그다음 바이트 예산에 맞춰 마지막 사용이 가장 오래된 것을.
   그래서 수명이 없는 엔트리도 여전히 유계다 — 용량에 의해. 그것이 바로 셀프호스티드 엔진의
   프리픽스 캐시가 갖는 계약이다. `TestUntilEvictedIsStillBoundedByBytes`가 그것을 단언하고
   예산에 실제로 도달했음도 단언하므로, 공허하게 통과할 수 없다.

2. **`dorang_passthrough_requests_total`에는 증가 지점이 있다.** `internal/server/passthrough.go:211`이고,
   그 파일이 쓰인 때부터 있었다. `docs/OPERATIONS.md`와 그 한국어 미러는 없다고 적었다.
   카운터와 그것에 관한 주장이 어긋난 것은 어느 쪽도 단언하는 것이 없었기 때문이다. 문서는
   정정했고, `TestPassthroughCounterIsIncrementedAndExported`가 이제 둘을 붙들어 맨다.

3. **`metering_degraded`에는 이미 메트릭이 있었다.** `docs/CONFIG.md` §23.1과
   `docs/OPERATIONS.md`는 "메트릭 없음, health 필드 없음, admin 필드 없음"이라고 적었다.
   메트릭은 §12.3 표면과 함께 들어왔는데 문서를 다시 보지 않은 것이다. 아직 없던 것은
   health 필드뿐이었다. (2)와 같은 형태다: 산문이 유일한 기록이었고, 산문은 빌드를
   깨뜨리지 않는다.

4. **로드되고 아무것도 하지 않는 설정 열한 개가 더 있다.** 재발 가드가 첫 실행에서 찾아냈고
   `internal/config/consumed_test.go`의 `knownUnwired`에 적혀 있다:
   `providers[].usage_probe`(§6.2, 페처는 존재하는데 아무것도 그것을 생성하지 않는다 —
   **2026-07-29 종결**, 위의 제어 표를 보라), `providers[].metrics.interval`(§12.4 —
   **이제 `metrics` 블록 전체가 로드 에러다**: 아무것도 백엔드를 스크레이프하지 않으므로
   무력하게 두는 대신 거부하며, 스크레이프가 필요 없는 전략들을 이름으로 알려 준다),
   `providers[].params.drop`과 `.drop_unsupported`(§10.3 — *어느* 파라미터를 드롭할지
   말하는 두 노브가 변환 경로에 닿지 않는다), `routing.prefix.checkpoints`(§7.4b),
   `models[].deployments[].stream_timeout`,
   `key_rotation.providers[].affinity_group`(검증조차 되지 않는다),
   `cluster.redis_url_env`(§13 — `capacity_mode: shared-redis`에 필수인데 Redis 클라이언트를
   생성하는 곳이 어디에도 없다), `observability.otlp_endpoint`, `.log_level`,
   `.log_format`. 어느 것도 여기 범위에 있지 않았다. 각각은 이제 산문이 아니라 테스트를
   깨뜨리는 자리에 기록돼 있다.

5. **`router.Config.PinnedWait`은 설정에서 한 번도 설정되지 않는다.** 그것은 블록할 수 있는
   유일한 인터랙티브 경로를 통제하므로, 그 경로는 죽어 있었다. 이제 `PinnedWait`이 0이면
   principal의 `max_queue_wait`이 예산을 공급하며, 애초에 그 설정에 인터랙티브 소비자를
   준 것이 바로 그것이다.

## 재발 가드, 그리고 그것이 잡지 못하는 것

`TestEveryConfiguredFieldIsReadSomewhere`(`internal/config/consumed_test.go`)는 리플렉션으로
`config.Config`를 순회하며 `yaml` 태그를 지닌 모든 필드를 모으고, 각각의 Go 이름이
`internal/config` **바깥**의 비테스트 소스에 식별자로 등장할 것을 요구한다. 탈출구는 둘이고,
둘 다 침묵기가 아니라 주장이다: `readExempt`는 효과가 전부 이 패키지 안에서 끝나는
필드(비밀 소스는 여기서 해소되고 값으로 떠난다)나 거부를 나르려고 존재하는 필드를 위한
것이고, `knownUnwired`는 위의 장부를 위한 것이다. 장부는 **양방향**으로 검사된다 — 소비자가
없는 새 설정도 실패하고, 그 사이에 연결된 항목도 실패한다. 그래서 목록이 낡을 수 없다.

이것은 바닥이지 증명이 아니다:

- **읽힌 다음 버려지는 값은 보지 못한다.** `Decision.PriorityTier`는 계산된 뒤 버려졌고,
  그 사슬의 모든 이름은 "참조됨"이었다. 참조는 필요조건이지 충분조건이 아니다. 동작을
  단언하는 것은 그 옆의 설정별 테스트들이다.
- **짧은 필드 이름에는 공허하다.** `Enabled`, `Path`, `Drop`, `Interval`, `Timeout`은
  어디에나 나오므로, 이 일제 점검이 연결되지 않았다고 확인한 설정 넷 —
  `providers[].metrics.interval`, `providers[].params.drop`,
  `providers[].usage_probe.interval`, `models[].deployments[].stream_timeout` — 은 가드에
  보이지 않으며 대신 `docs/CONFIG.ko.md` §23.1에서 추적된다. 구별되는 이름에는 성립하고,
  새 설정이 떨어지는 자리가 바로 거기다: `MaxQueueWait`, `ClientPriority`, `PrefixTTL`,
  `AffinityGroup`.
- **의미론에 대해서는 아무 말도 하지 않는다.** `MaxQueue`를 읽어 엉뚱한 값과 비교해도
  통과한다.
- **`config.Config`만 덮는다.** 설정 필드가 아닌 제어 — `Decision.PriorityTier`,
  `auth.Access.ObservedRPM` — 는 사정거리 완전히 바깥이다. 결함 유형의 그 절반에는 자동
  가드가 없고, 정직한 답은 그것을 찾아내는 유일한 수단이 소유 패키지 혼자서는 만들어 낼 수
  없는 관측값을 단언하는 조립된 스택 테스트라는 것이다.

## 테스트에 관하여

이 결함 유형에서는 검사가 동작함을 증명하는 테스트가 아무것도 증명하지 못한다: 기존
스위트들은 레이트 한도에 대해서도, 긴급도 스코어러에 대해서도, 우선순위 클램프에 대해서도
이미 그것을 했고, 셋 다 도달 불가능했다. 그래서 여기 추가된 모든 테스트는 검사를 소유한
패키지 바깥의 관측값을 단언하거나 — HTTP 상태, 헤더, health 본문, `Snapshot` 카운터 —
아니면 이음매 자체를, 즉 설정된 값이 그것에 작용하는 서브시스템에 도달했음을
단언한다(`Router.Rotation()`, `Broker.WaitBudget()`, `PriorityConfig.GrantsHint()`).

둘은 동작이 아니라 구조를 보며, 의도적으로 그렇다.
`TestDockerfileHealthcheckInvokesARealSubcommand`는 이미지 자신의 `HEALTHCHECK` 줄을 읽어
그 인자를 실제 CLI 디스패처에 먹인다. "CLI에 health 명령이 있다"가 틀렸던 지점이었던 적은
한 번도 없기 때문이다 — 틀린 것은 두 쪽이 서로 어긋난다는 사실이었다.
`TestStateDirIsDeclaredAndRead`는 이미지의 `ENV DORANG_STATE_DIR`이 그 `VOLUME`이 선언하는
디렉터리를 지목할 것을 요구한다.

---

# 처리, 3차 — 병합, 그리고 무엇이 검증됐는가

아홉 개의 수정은 위의 "# 처리" 절과 함께 한 브랜치에서 작성됐다.
**문서만 `main`에 도달했다** — 제목이 *"gitignore: cmd/ was never in version
control"*인 커밋 안에서. 5,221줄의 수정 코드는 브랜치에 남았고, 그 브랜치는
`main`의 조상이 아니다. 그래서 그 커밋이 살아 있는 동안 `main`은 바이너리에 없는
보호를 주장하는 보안 문서를 출하했다 — 아홉 개의 결함이 **Closed**로 표시되고,
존재하지 않는 파일과 이 문서 안에만 나타나는 테스트를 이름으로 대면서.

감사가 열두 개의 처리 주장을 전부 검사했고 **`main`에서 열 개가 거짓**임을 찾았다.
이 절은 브랜치를 병합한 뒤에 쓰였으며, 권위 있는 진술이다: **아래의 모든 주장은 그
자리에서 수정을 되돌리고, 지정된 테스트를 돌리고, 그것이 깨지는 것을 관찰해서
검사했다** — 그런 다음 복원하고 통과하는 것을 관찰했다. 코드가 올바르게 읽힌다는
근거만으로 종결로 기록된 것은 하나도 없다. 있으나 없으나 통과하는 수정은 들어온 적이
없는 것이다.

위의 두 절은 역사로 읽어라. 그것들이 이 절과 어긋나는 곳에서는 이 절이 바이너리가
하는 일이다. 구체적인 정정은 끝에 나열한다.

## 병합: 어느 구현을 남겼고, 왜인가

`main`은 멀리 나아가 있었다 — `internal/backend`로의 L5 추출이 완료됐고,
`internal/app`은 더 이상 HTTP 호출을 하지 않으며, "설정됐지만 한 번도 적용되지 않음"
일제 점검이 들어왔고, 키 회전은 두 번째 테이블을 얻었다. 열두 개 파일이 충돌했고
그중 몇은 **한 가지 것의 구현 둘**을 담고 있었다. 이 코드베이스는 바로 그것에 세 번
물렸으므로, 매 경우 하나를 남기고 다른 하나를 삭제했다. 플래그 뒤에 둔 것은 없다.

| 중복 | 남긴 것 | 삭제한 것 | 이유 |
|---|---|---|---|
| **레이트 윈도우** — `internal/app/rates.go`가 양쪽에 있었다: 브랜치의 `rateMeter`(텀블링 1분, 롤오버 때 맵을 통째로 버림, 주체별)와 일제 점검의 `keyRates`(샤드 16개, 타임스탬프가 찍힌 1초 버킷 60개, 콜드 축출을 갖춘 100 000 주체 상한, **키**별) | `keyRates` | `rateMeter` | 롤링 1분은 운영자가 프로바이더 자신의 429와 대조할 수 있는 숫자다. 텀블링은 경계를 걸친 버스트를 거부하고 걸치지 않은 버스트는 허용한다. 그러나 `keyRates`는 api key만으로 키잉돼 있었고, 그것이 아래의 살아 있는 결함이다 — 그래서 **주체별로 재도출**해 `kind:id`로 키잉하고 `auth.RateSource`를 서브하게 만들었다. 자료구조 하나, 주체 셋 |
| **`MostRestrictiveParallel` / `strictest` / `mostRestrictive`** — 같은 "두 상한 중 작은 쪽, 0은 없음을 뜻함"을 세 번 쓴 것 | `auth.MostRestrictiveParallel`(공유 규칙)과 `capacity.strictest`(브로커 자신의 것으로, 일제 점검이 추가한 큐 상한도 함께 나른다) | `capacity.mostRestrictive`, 그리고 `app.principal.maxParallel`의 인라인 루프 | 브랜치의 `mostRestrictive`는 큐 지원이 빠진 `strictest`와 바이트 단위로 동일했다. `principal.maxParallel`은 이제 §11.2의 규칙을 네 번째로 받아쓰는 대신 `auth.MostRestrictiveParallel`을 호출한다 |
| **동시성 상한이 브로커로 가는 경로** — 브랜치는 `principalMaxParallel(rq.Principal)`을 통해 라우팅 요청 리터럴에서 `capacity.Request.PrincipalMax`를 설정했고, 일제 점검은 우선순위 클래스·클라이언트 힌트와 나란히 `applyPrincipalPolicy`에서 설정했다 | `applyPrincipalPolicy` | `principalMaxParallel`, 그리고 리터럴의 그 필드 | 한 필드에 쓰는 곳이 둘이었고, 나중 것이 조용히 이겼다. 이제 한 곳이 호출자의 정책을 라우팅 요청으로 읽어 넣는다 |
| **`internal/capacity/principalmax_test.go`**(브랜치) 대 `queue_limits_test.go`의 `TestPrincipalMaxTightensTheAxis` / `TestPrincipalMaxNeverWidens`(일제 점검) | 일제 점검 쪽 | 브랜치의 파일 | 같은 세 성질이고, 일제 점검 쪽은 헬퍼가 아니라 브로커를 종단 간으로 훑는다 |
| **상한 있는 업스트림 읽기** — `internal/app/dispatch.go`의 `readUpstreamBody` + `DefaultMaxResponseBytes` + `errUpstreamTooLarge`(브랜치), 그리고 `internal/backend/backend.go`의 같은 것(브랜치, 재도출) | `internal/backend` 쪽 | `internal/app` 쪽, 그리고 `TestOversizedUpstreamResponseIsRefused`의 `internal/app` 사본 | `internal/app`은 더 이상 HTTP 호출을 하지 않는다. 그 사본에는 프로덕션 호출자가 없었다 — 이미 아무것도 실행하지 않는 판본이었고, 그것이 이 검토의 이름이 된 바로 그 결함이다 |
| **§11.3 스크러버의 적용 지점** — `internal/redact`로 스크러빙하는 `internal/app/dispatch.go`(브랜치)와 `collectSecrets`로 스크러빙하는 `internal/backend/errors.go`(브랜치, 재도출) | `internal/backend` 쪽 | `internal/app` 지점과 그 `redact` import | 같은 이유. `collectSecrets`는 더 강하기도 하다: 아웃바운드 헤더에 실제로 얹힌 것을 읽으므로, 자신이 소유하지 않은 코드가 적용한 OAuth 토큰도 잡힌다 |
| **배치 행의 허용목록 검사** — `ownerResolver.identity`가 `principalAllowsModel(p, model)` **와** `p.Authorize(auth.Access{Model: model})`를 둘 다 물었고, `auth.Limits.authorize`는 `Access.Model`이 비어 있지 않으면 이미 허용목록을 참조한다 | `p.Authorize`(공유 게이트) | `principalAllowsModel` | 어느 한쪽만 지워도 관찰 가능한 변화가 없었으므로, "수정을 되돌리고 테스트가 깨지는지 본다"가 다른 사본이 조용히 덮고 있던 결함에 대해 통과해 버렸다. 그것이 이 차수가 없애려고 존재하는 바로 그 실패 양식이다 — 1차에 "구현이 둘인 통제 하나"로 기록됐고, 이제는 구현이 하나인 통제 하나다 |
| **키의 비밀 교체** — `store.ReplaceKeyVerifier`(브랜치)와 `store.RotateKey` + `EndGrace`(`secrets.go`, 현재 `main`) | `RotateKey` + `EndGrace` | `ReplaceKeyVerifier` | 취향의 선택이 아니다. `ReplaceKeyVerifier`는 `api_keys.lookup`, `.token_hash`, `.hash_scheme`에 썼다 — **비정규화된** 사본이다. 인증은 `api_key_secrets`(`resolveKeyQuery`)를 통해 해석되므로, 현재 `main`의 스키마에서는 유출된 비밀이 계속 인증에 성공하고 갓 발급된 것은 게이트웨이가 모르는 채였을 것이다 — 크리덴셜이 교체됐다고 운영자에게 말하는 `200 OK` 뒤에서. `/key/regenerate`는 이제 유예 0의 `RotateKey`에 이어 `EndGrace`다: 계획된 회전과 사고의 차이는 파라미터이지 두 번째 쓰기 경로가 아니다 |
| **모델 허용목록 거부의 상태 코드** — 브랜치의 `Request.AuthorizeModel`은 §7.2를 근거로 **401**로 답했고, 일제 점검은 같은 거부를 §11.2를 근거로 401에서 **403 `permission_error`**로 막 고쳐 놓은 참이었다 | 403 | 401 | §7.2는 **태그 라우팅** 미스에 관한 것으로, 다른 조건이다. 공유 인가 게이트는 이 거부에 대해 언제나 403으로 답해 왔으므로, 규칙 하나에 답이 둘이었다. 패스스루의 두 거부(`model_not_allowed`와 `model_not_authorizable`)도 함께 옮겼다. 다시 인코딩해야 할 본문을 두고 호출자에게 재인증하라고 말할 수는 없기 때문이다 |
| **업스트림 컨텍스트 오버플로 분류기** — `internal/app/estimate.go`와 `testing/scenario/harness.go`의 두 번째 사본, 둘 다 `server.Error.Message`를 읽는다 | 둘 다, `NativeMessage`를 읽도록 고쳐서 | — | 두 사본은 남는다(하네스는 다른 에이전트의 파일이고 작업 중이다). 그러나 둘 다 결함 3에 의해 조용히 깨져 있었다: `Message`는 이제 `canonicalMessage(status)`, 즉 상태 줄만의 함수이므로, 거기서 벤더의 오버플로 문구를 훑어 봐야 아무것도 나오지 않는다. §10.5a의 "더 큰 윈도우로 라우팅"은 업스트림 신호로부터 도달 가능하기를 그만둔 상태였다. **이것이 이 차수가 접지 않은 유일한 중복**이며, 다음 차수가 접도록 여기 이름을 적는다 |

### 재도출 — 수정이 지키던 코드가 옮겨 갔기 때문에

브랜치가 병합되지 않은 채 있는 동안 `main`은 L5 추출을 완료했다: `internal/app`은
이제 모든 업스트림 호출을 `internal/backend`에 위임한다. 따라서 네 개의 수정은 한
패키지 아래로 들어가고, 하나는 양쪽 다 예상하지 못한 곳으로 들어간다.

- **§11.3 유출 → `internal/backend/errors.go`.** `upstreamError`가 프로바이더
  크리덴셜을 `Error.NativeMessage`에서 스크러빙한다. 업스트림의 텍스트가 이제 사는
  곳이 거기다 — `Normalize`는 어느 분기에서도 디코드된 본문의 어떤 것도
  `Error.Message`에 쓰지 않는다.
- **상한 없는 읽기 → `internal/backend/backend.go`.** L5 계층은 대화형 디스패처가
  가졌던 것과 같은 비대칭을 출하했다: 에러 분기에는 1 MiB, 성공 분기에는
  `io.ReadAll`. `Options.MaxResponseBytes`, `DefaultMaxResponseBytes`, 그리고
  재시도 불가한 `upstream_response_too_large`.
- **전송 에러 텍스트 → `internal/backend/errors.go`.** `transportError`는
  크리덴셜을 나를 수 없으므로 안전하다고 주장하는 주석 아래에서 `err.Error()`를
  그대로 중계했다. 맞는 말이고, 결함이 말한 바가 아니다: 그것은 운영자의 내부
  호스트명·포트·IP를 나른다.
- **배치 실행기 → `backend.Do`.** 브랜치의 판본은 자기 HTTP 호출을 했다. 예산 hold,
  행/본문 모델 불일치 검사, 정산이 이제 대신 `backend.Do`를 감싸므로, 배치 경로와
  대화형 경로가 클라이언트 하나·스크러버 하나·상한 있는 읽기 하나를 공유한다.
- **배치 소유자의 봉투 → `cluster.AuthPrincipal`.** 브랜치는 자기
  `recordFromAPIKey`를 갖고 있었고, `main`은 그 변환을 `cluster.AuthRecord`로 옮겨
  놓았다. `AuthRecord`는 키의 **티어**도 해석해서 한도에 적용한다. 두 호출자가 하나의
  변환에서 도출하도록 `AuthRecord`를 쪼갰다 — 그러지 않았다면 배치 행은 티어 없는
  봉투에 대해 인가됐을 것이고, 그것은 한 크리덴셜의 한도에 대한 두 번째 시야이며
  R1-A가 이름 붙인 형태다.

### 문서가 종결됐다고 주장한, 살아 있는 결함

**팀의 `rpm_limit`이 그 팀 아래 키 개수만큼 곱해지고 있었다.** `auth.Access`는 세
주체에 대해 관측된 쌍을 하나만 날랐고, `internal/app`의 윈도우는 api key만으로
키잉돼 있었으므로, `internal/auth/principal.go`는 **팀**의 상한을 한 **키**의 카운터와
비교했다. 수정 전에 실증했다: 팀 `rpm_limit: 4`, 키 열 개 × 요청 네 개 →
**거부 0**. 한 키에 요청 열 개 → 여섯 개 거부. 결함 4와 같은 N배 곱셈이 레이트
차원에서 일어난 것이며, 이 문서가 결함 2를 종결이라고 말하는 동안 `main`에서 살아
있었다.

`auth.RateSource`로 종결: `Limits.authorize`가 주체의 **id**를 받아 그 주체의
카운터를 읽고, `keyRates`는 `kind:id`로 키잉되며, 윈도우는 요청당 한 번 진입한다 —
주체의 샤드 락 아래에서 읽기와 증가를 함께 하므로, 동시 요청 둘이 같은 사전 카운트를
둘 다 관측할 수 없다. 토큰은 정산 시점에 세 주체 전부에 얹히며, 그 출처는
`App.recordMetrics`, 즉 유일한 프로덕션 정산 지점이자 끝난 모든 요청이 통과하는
유일한 지점이다.

## 검증: 수정을 되돌리고 테스트가 깨지는지 본다

모든 행은 기계적으로 만들었다: "되돌린 것" 열의 편집을 적용하고, 지정된 테스트만
돌리고, 결과를 기록하고, 복원한다. 수정을 되돌렸을 때 테스트가 **깨졌고** 수정이 있을
때 통과한 경우에만 그 행이 있다.

| # | 통제 | 되돌린 것 | 지정된 테스트 |
|---|---|---|---|
| 1 | 배치: 디스패치 시점 허용목록과 킬 스위치 | `ownerResolver.identity`가 더 이상 `p.Authorize`를 호출하지 않음 | `TestBatchExecutorRefusesAModelTheOwnerMayNotUse`, `TestBatchExecutorRefusesABlockedOwner` |
| 1 | 배치: 모든 행에 예산 hold | `reserveBatch`를 nil hold로 교체 | `TestBatchExecutionIsBudgeted` |
| 1 | 배치: 업로드 시점 허용목록, 행별 | `validateRow`가 `vc.authorize`를 건너뜀 | `TestUploadRefusesARowNamingADisallowedModel` |
| 2 | 관측된 윈도우가 애초에 검사에 도달함 | `principal.Authorize`가 더 이상 `rates.observe`를 호출하지 않음 | `TestRPMLimitIsEnforced`, `TestTPMLimitIsEnforced`, `TestRateWindowRolls`, `TestRPMLimitRefusesThroughTheWholeStack`, `TestTPMLimitCountsFinishedTokens` |
| 2 | 윈도우가 키만이 아니라 모든 주체를 셈 | `subjectsOf`가 키 하나만 반환 | `TestTeamRPMCountsEveryKeyUnderTheTeam` |
| 2 | `auth`가 각 주체를 그 주체 자신의 카운터와 비교 | `Limits.authorize`가 다시 `Access.ObservedRPM/TPM`을 읽음 | `TestRPMLimitIsEnforced`, `TestTeamRPMCountsEveryKeyUnderTheTeam` |
| 2 | 토큰 쪽 절반이 실제로 끝난 요청에서 정산됨 | `App.recordMetrics`가 `recordTokens`를 뺌 | `TestTPMLimitCountsFinishedTokens` |
| 2 | 정산된 토큰이 모든 주체에 얹힘 | `recordTokens`에 키 id만 넘김 | `TestSettledTokensReachEverySubjectOfTheRequest` |
| 2 | `max_parallel_requests`가 라우팅 요청까지 도달 | `applyPrincipalPolicy`가 `PrincipalMax = 0`으로 설정 | `TestMaxParallelReachesThePrincipal` |
| 2 | 브로커가 실려 온 상한을 적용 | `Broker.needs`가 `strictest(…, req.PrincipalMax)`를 뺌 | `TestPrincipalMaxTightensTheAxis`, `TestPrincipalMaxNeverWidens` |
| 3 | 봉투가 업스트림의 말이 아니라 dorang의 말을 나름 | `Normalize`가 `e.Message = e.NativeMessage`로 설정 | `TestNormalizeEveryUpstreamShape`(그리고 `internal/server/errorleak_test.go`) |
| 3 | 기록되는 네이티브 텍스트 자체에서 보낸 비밀이 스크러빙됨 | `upstreamError`가 `scrub(e.NativeMessage, secrets)`를 뺌 | `TestAHostileUpstreamCannotEchoTheCredentialBack` |
| 3 | 전송 에러 텍스트가 내부 호스트를 이름으로 대지 않음 | `transportError`가 `err.Error()`를 중계 | `TestUnreachableUpstreamDoesNotNameInternalHosts` |
| 3 | `x-dorang-native-error-type`이 클램프되고 헤더에 안전함 | `WriteError`가 날 것의 `NativeType`을 내보냄 | `TestNativeErrorTypeHeaderIsCheckedAndClamped` |
| 4 | 예산을 선언한 주체마다 hold 하나 | `budgetSubjectsOf`가 키 하나만 반환 | `TestTeamBudgetIsNotMultipliedByTheNumberOfKeys`, `TestEachBudgetSubjectKeepsItsOwnPeriod` |
| 5 | prefix 체인이 테넌트로 시드됨 | `NewChain`이 다시 `H(group)`으로 시드 | `TestChainIsTenantScoped`, `TestTenantAndGroupCannotBeConfused` |
| 5 | `router.Request.Tenant`가 프로덕션에서 할당됨 | `decode`가 `Tenant: ""`로 설정 | `TestRouterRequestCarriesTheTenant` |
| 6 | 미지 키 플러드가 스냅샷 병합을 몰아붙일 수 없음 | `insert`가 `positives+negatives`로 병합 | `TestUnknownKeyFloodDoesNotMergeOnEveryMiss` |
| 7 | 비스트리밍 업스트림 본문에 상한이 있음 | `readUpstreamBody`가 `io.ReadAll(r)`을 반환 | `TestOversizedUpstreamResponseIsRefused` |
| 8 | 패스스루가 허용목록을 참조 | `authorizePassthroughModel`이 대신 요청을 인가됨으로 표시 | `TestPassthroughEnforcesTheModelAllowList` |
| 8 | 패스스루가 모델을 판별할 수 없을 때 닫히는 쪽으로 실패 | `principalRestrictsModels`가 false를 반환 | `TestPassthroughRefusesWhenTheModelCannotBeDetermined` |
| 8 | `ModelAuth` 답 없이는 라우트가 mux에 도달할 수 없음 | `newRouteTable`이 `ModelAuthUnset` 거부를 그만둠 | `TestRouteTableRefusesAnUndeclaredModelAuth`, `TestEveryRouteDeclaresAModelAuthMode` |
| 8 | 한 번도 묻지 않은 `ModelAuthHandler` 라우트는 요청을 실패시킴 | `Server.serve`가 사후조건을 뺌 | `TestModelAuthHandlerRouteThatSkipsTheCheckFails` |
| 9 | `Credential.MarshalYAML`이 인라인 리터럴에 얹힐 곳을 남기지 않음 | 메서드 이름을 바꿔 인터페이스에서 빼냄 | `TestMarshalingNeverEmitsAPlaintextSecret` |
| 9 | `RotationKey.MarshalYAML`, 회전 풀에 대해 같은 것 | 메서드 이름을 바꿔 인터페이스에서 빼냄 | `TestMarshalingNeverEmitsAPlaintextSecret` |
| 9 | `import config`가 인라인 리터럴이 아니라 `key_env`를 씀 | `internal/config/import.go`가 리터럴에 대해 다시 `SecretRef{Inline: raw}`를 내보냄 | `TestImportWarnsAboutALiteralKey` |
| — | 패스스루가 업스트림의 `Set-Cookie`를 중계하지 않음 | `dst.Del` 호출 두 개 제거 | `TestPassthroughDoesNotRelaySetCookie` |
| — | 소유자 없는 배치 레코드가 모두의 것이 아님 | `ownedBy`가 다시 빈 `recordOwner`를 허용 | `TestAnUnownedRecordIsNotVisibleToEveryKey` |
| — | 마스터 크리덴셜이 자기가 만든 것을 소유 | `principalID`가 마스터에 대해 다시 `""`를 반환 | `TestTheMasterCredentialOwnsWhatItCreates` |
| — | `/key/regenerate`가 실제로 유출된 비밀을 퇴역시킴 | `ReplaceVerifier`를 no-op으로 만듦 | `TestRegeneratingAKeyRetiresTheOldSecret` |
| — | 업스트림 오버플로가 다름 아닌 네이티브 텍스트로부터 분류됨 | `upstreamCause`가 `e.Message`를 읽음 | `TestAnUpstreamOverflowIsRecognisedFromTheBody` |
| — | 허가되지 않은 우선순위 힌트가 드롭됐다고 보고까지 됨 (§10.5) | `droppedParams`가 그 헤더를 이름으로 대기를 그만둠 | `TestGrantedClientPriorityIsHonouredAndADropIsReported` |

### 쓰여 있던 대로는 검사를 통과하지 못해 고친 테스트 넷

- **`TestOversizedUpstreamResponseIsRefused`는 상한이 아니라 거부를 증명했다.**
  `readUpstreamBody`를 `io.ReadAll(r)`로 되돌려도 여전히 too-large 에러를
  반환했다. 길이 검사가 읽기 뒤에 왔기 때문이다 — 그래서 그 테스트는 안전장치가
  전혀 없는 구현에 대해 통과했다. 결함이 말하는 것은 원격 OOM이다: 그 바이트를
  읽는 일 자체가 없어야 한다. 살아남은 사본은 리더에게 요청된 양을 센다.
- **`TestTPMLimitCountsFinishedTokens`는 카운터를 자기가 먹였다.**
  `rates.record(keyID, 500)`을 직접 호출하고 429를 단언했다. 그것은 비교가
  동작함을 증명하고, 프로덕션의 무언가가 그것을 먹이는지에 대해서는 아무 말도 하지
  않는다 — `rpm_limit`과 `tpm_limit`이 프로젝트 내내 아무것도 강제하지 않으면서
  단위 테스트를 갖춘 채로 출하되게 만든 바로 그 위상이다. 이제는 사용량을 보고하는
  실제 업스트림에 대해 실제 chat completion을 몰고, 두 번째 요청이 거부된다.
- **`TestGrantedClientPriorityIsHonouredAndADropIsReported`는 스스로를 종단 간이라
  이름 붙이고 `CanonicalFor`를 직접 호출했다.** §10.5의 요구사항은 드롭된 힌트가
  `x-dorang-dropped-params`에 *보고*되는 것인데, 그 헤더를 단언하는 것은 아무것도
  없었다. 이제는 허가되지 않은 키에서 `X-Request-Priority`를 조립된 스택을 통해
  보내고 응답 헤더를 읽는다.
- **결함 9의 수정은 1차가 말한 자리에 있지 않다.** `SecretRef.MarshalYAML`은 이름을
  바꿔 없애 버려도 모든 마셜링 테스트가 그대로 통과한다: 두 쓰임 모두
  `yaml:",inline"`로 `SecretRef`를 임베드하고, gopkg.in/yaml.v3는 인라인 임베드된
  구조체의 노출 필드를 평탄화할 뿐 그 `MarshalYAML`을 호출하지 않는다. 그 메서드는
  **죽은 코드**다 — 올바르게 쓰였고 한 번도 호출되지 않는다. 실제로 하중을 받는 쪽은
  `Credential.MarshalYAML`, `RotationKey.MarshalYAML`, 그리고 인라인 리터럴을 만들기를
  거부하는 importer이며, 그것이 위 표의 세 행이다.

## `internal/admin` — 마운트됨

1차는 그것을 마운트하면 잠재 결함 셋이 살아 있는 결함이 된다는 근거로 아무데도
연결하지 않은 채 두었다. 셋 다 종결됐고 마운트돼 있으므로, **유출된 키를 폐기할 API
경로가 있다** — 검토에서 가장 평이한 문장이 없다고 말한 바로 그것이다.

- **인증 전 읽기.** `API.ServeHTTP`는 무엇보다 먼저 인증한다: 라우트 조회 전에,
  `OPTIONS` 전에, 405 전에, 501 전에. 그 위에 있던 모든 것이 오라클이었다. 테스트:
  `TestTheRouteTableIsNotReadableBeforeAuthentication`.
- **팀 범위 관리.** `admin.Scope`는 역할이 아니라 키의 `team_id`에서 도출된다 —
  누구도 설정하기를 잊을 수 없는 값이다. 영값 `Scope`는 아무것도 허용하지 않는다.
  테스트: `TestATeamAdminCannotReachAnotherTeam`,
  `TestATeamAdminCannotMintOrMoveKeysAcrossTeams`,
  `TestDeploymentWideEndpointsRefuseAScopedAdmin`, `TestTheZeroScopeAdmitsNothing`.
- **요청이 제공한 id 필터.** 다른 팀을 이름으로 대는 파라미터는 `403 out_of_scope`다.
  범위 밖의 오브젝트 자체는 `404`이며, 존재하지 않는 id와 똑같으므로 어떤 id도 존재
  여부 오라클이 되지 않는다. 목록은 스토어가 반환하기 전에만이 아니라 반환한 뒤에도
  필터링된다. 테스트: `TestListingFiltersCannotWidenTheScope`,
  `TestSpendLogsCannotReachAnotherTeam`, `TestBudgetSubjectsAreScoped`.

`cmd/dorang/revoke_test.go`는 그 사고를 HTTP 위에서 종단 간으로 돌린다 —
`TestALeakedKeyCanBeRevokedThroughTheAPI`는 동작하는 키 하나를 호출 한 번으로
차단하고 그 키의 다음 요청이 거부되는 것을 지켜보며 감사 행을 되읽는다.
`TestAnOrdinaryKeyCannotAdminister`는 테넌트 키에 403, 키가 없을 때 401을 못 박는다.

`internal/store/admin.go`는 마운트된 표면이 호출하는 것을 공급한다: list, update,
delete, `users.role`, 그리고 저장소 최초의 `INSERT INTO audit_logs` — 첫 마이그레이션
이래로 스키마와 인덱스 둘, 그리고 거기서 DELETE 하는 보존 스윕까지 갖고 있던 테이블에
대해서. 회전과 pend는 `store.RotateKey`, `EndGrace`, `ListKeySecrets`, `PendKey`,
`ReleaseKey`에 연결돼 있어서 `/key/rotate`, `/key/rotate/cut`, `/key/secrets`,
`/key/pend`, `/key/release`가 501로 답하는 대신 서브한다. 의존물 일곱은 여전히 없고
빠진 조각을 이름으로 대며 `501 dependency_not_configured`로 답한다. 그 목록은
`docs/OPERATIONS.ko.md` §3.2에 있다.

## 앞의 두 차수가 틀린 것

위의 두 절은 이 문서 어디에도 거짓 주장이 서 있지 않도록 **그 자리에서 정정했다**.
그것들이 이전에 말한 바를 여기 기록한다. 각각의 오류가 이 검토가 다루는 결함 부류의
사례이고, 그것을 조용히 지우는 것은 두 번째 감사를 필요하게 만든 바로 그 실수를
되풀이하는 일이기 때문이다.

1. **`main`에 아예 없는 코드에 대해 "Closed"가 주장됐다.** 결함 아홉, 처리 주장 열둘,
   그중 열이 거짓 — 지어낸 것이 아니라, 그것이 서술하는 코드보다 앞서 복사된 것이다.
   파일이나 테스트를 이름으로 대는 처리는 그 파일이나 테스트가 **그것이 주장된 브랜치
   위에** 존재해야만 종결이며, 이것을 잡았을 검사는 diff를 읽는 것이 아니라
   `git merge-base --is-ancestor`다.
2. **결함 2는 두 번 종결됐고 두 번 다 틀렸다.** 1차는 존재하지 않는 `rates.go`를
   서술했다. 그 뒤 일제 점검이 실물을 하나 쓰고 결함 2를 종결로 기록했다 — 윈도우가
   api key만으로 키잉된 채로, 그래서 팀 상한은 여전히 그 팀 아래 키 개수만큼
   곱해졌다. 독립적인 "종결"이 둘인데 결함은 둘 다에서 살아남았다.
3. **결함 3의 수정이 틀린 패키지에 서술됐다.** `internal/redact`를 `internal/app`에서
   적용한다고 했다. L5 추출이 업스트림 호출을 옮겼고 스크러버도 함께 옮겨야 했다.
   같은 이전이 `internal/app/estimate.go`의 오버플로 분류기를 깨뜨렸는데, 아무도
   알아채지 못했다. 그것이 읽는 필드는 여전히 존재했고 단지 그 텍스트를 나르기를
   그만뒀을 뿐이기 때문이다.
4. **결함 7의 수정이 `dispatchState` 위에 서술됐다.** 그것은 더 이상 HTTP 호출을 하지
   않으며, 그 테스트는 상한이 아니라 거부를 증명했다.
5. **결함 9의 수정이 `SecretRef.MarshalYAML`이라고 서술됐다.** 그것은 죽은 코드다 —
   `yaml:",inline"`는 임베드된 구조체의 노출 필드를 평탄화하고 그 마셜러를 결코
   호출하지 않는다. 결함이 이름으로 댄 그 메서드는 지금도 한 번도 실행되지 않는다.
6. **`internal/admin`이 의도적으로 아무데도 연결되지 않은 것으로 기록됐다.** 운영상
   귀결로 "키를 폐기할 API 경로가 없다"를 달아서. 쓰일 당시에는 참이었고 지금은
   아니다.
7. **`batch.ownedBy`가 "이 차수에서 고쳐지지 않음"으로 기록됐고**, 실제로는 그 차수가
   쓰인 브랜치에서 이미 고쳐져 있었다.

## 여전히 미해결

이름을 대는 이유는, 존재하지만 호출되지 않는 통제가 이 코드베이스의 지배적 결함이고
이제 그것이 세 번 세어졌기 때문이다.

**`8e6016d` 기준, 2026-07-29.** 이 목록에 날짜를 붙인 이유는 날짜 없는 백로그가 바로
이 문서가 쓰인 대상이기 때문이다: 아래 모든 항목은 그 커밋의 blob에 대해 재검사했고,
이전에 재검사되지 *않았던* 항목 셋은 며칠째 거짓이었다. 이것을 읽을 때는 먼저
`git log`로 날짜를 대조하라 — 백로그는 트리에 대한 측정이고, 스탬프 없는 측정은
감사할 수도 신뢰할 수도 없으며 오직 믿을 수 있을 뿐이다.

1. **미지 키에 대한 스토어 조회는 여전히 상한이 없다**(결함 6의 후반부). 인증되지
   않은 요청마다 `LoadByLookup` 하나. 네거티브 TTL은 같은 키의 반복을 제한하고, 서로
   다른 키를 제한하는 것은 없다. 1차가 이것을 미해결로 남은 것 중 가장 큰 것이라
   불렀고, 지금도 그렇다.
2. **`internal/store`에 모델 레지스트리가 없다.** `deployments`와 `model_aliases`는 Go
   코드가 없는 테이블이므로 `/model/*`와 `/model_group/info`는
   `501 dependency_not_configured`로 답한다. 이유는 "스토어 계층이 없다"보다 더
   조심해서 진술해야 한다: 그 두 테이블에는 *읽는 쪽*도 없다. 라우팅은 설정 파일에서
   컴파일되므로, 거기에 쓴다고 해서 어떤 라우팅 결정도 바뀌지 않는다.

   *(이 항목은 `users`, `teams`, `team_members`와 예산 상한도 이름으로 댔는데, 그
   절반을 닫는 과정에서 스토어 계층 부재보다 더 나쁜 것이 나왔다. 테이블은 더 작은
   장애물일 뿐이었다: `cluster.AuthPrincipal`은 **키**의 한도로 `auth.Principal`을
   만들고 `User`와 `Team`을 nil로 두었으므로, `users.blocked`, `teams.blocked`, 그
   상한들과 팀 레이트 한도는 어떤 노드에서도 어떤 지연에서도 아무 결정에 도달하지
   못했다 — **팀 수준** `rpm_limit`은 저장된 행에 올릴 수조차 없었고, 올렸더라도
   강제되지 않았을 것이다. 그것이 무해해 보였던 것과 같은 이유로 보이지 않았다:
   소유자 봉투를 읽는 모든 가드는 nil 가드를 갖고 있고, 소유자 없는 키에는 봉투가
   없으므로 그것이 옳다. 이제 크리덴셜 읽기는 같은 문장에서 두 소유자를 LEFT join
   하고, `AuthPrincipal`은 그것들을 필수 인자로 받으며, 디렉터리와 예산 라우트가
   연결돼 있다 — DESIGN W12 참조. 주체별 레이트 상한은 여전히 게이트에서 증명된다 —
   `TestTeamRPMCountsEveryKeyUnderTheTeam`,
   `TestSettledTokensReachEverySubjectOfTheRequest` — 그리고 이제는 저장된 행으로부터도
   도달 가능하다.)*
3. **`/audit/list`**: 행은 모든 관리 변경이 쓰지만 여전히 API로 읽을 수 없다.
   `internal/admin/routes.go`가 그것을, 이유가 거부를 서술하는 대신 "until this
   ships"라고 말하는 유일한 스텁으로 이름 붙인다. *(이 항목은 "**범위 집계 원장 질의가
   없어서** `/global/spend/report`와 세 개의 일일 활동 엔드포인트가 호출할 것이 없다"고
   말했다. 양쪽 절반 모두 거짓이었다 — 아래 4차 정정 참조.)*
4. **`quota.Registry`, `internal/probe`, `pricing.Catalog`, `App.Reload`**는 관리
   표면에서 도달할 수 없으므로 `/admin/credentials/health`, `/admin/quota`,
   `/spend/calculate`, `/admin/pricing/preview`, `/admin/config/reload`에 어댑터가
   없다. `SIGHUP`은 동작한다.
5. **업스트림 오버플로 분류기 사본 둘**이 `internal/app/estimate.go`와
   `testing/scenario/harness.go`에 남아 있다 — 그러나 중복은 이제 거의 없는 수준이고
   이 항목은 **그대로 나른 것이 아니라 강등된 것**이다: 둘 다 단일한
   `router.ClassifyBody`에 위임하는 세 줄짜리 어댑터이므로 규칙은 단일 출처이고 인자를
   섞는 부분만 다르다. 이 항목이 서술했던 것 — 한 분류에 대한 독립적인 구현 둘 — 은
   더 이상 존재하지 않는다.
6. **레이트 상한은 프로세스별이다.** N 노드 배포는 모든 `rpm_limit`과 `tpm_limit`을
   N배로 강제하며, 토큰 수는 정산 전까지 존재하지 않으므로 `tpm_limit`은 그다음
   요청을 제한한다.
7. **`auth.Strip`과 `store.ImportKeys`** — 1차에서 변동 없음. `auth.Strip`
   (`internal/auth/header.go`)은 테스트 호출자만 있다. 프로덕션의 strip은
   `server.StripAuthHeaders`다. `store.ImportKeys`에는 CLI도 HTTP 진입점도 없다.
   *(이 항목은 **OAuth 서브시스템**, **`internal/luaext`**, **`quota.Ranker`**도
   이름으로 댔다. 셋 다 연결돼 있다 — 아래 4차 정정 참조. 한 줄에 있던 다섯 이름 중
   셋이 거짓이었다.)*
8. 위의 "## 다루지 않은 것" 아래에 있는 것 중 이 항목 앞의 목록에서도, 아래 4차
   정정에서도 정정되지 않은 모든 것.

## 3차가 틀린 것 — 2026-07-29, `8e6016d`에서

위의 "여전히 미해결" 목록은 **그 자리에서 정정했고**, 각 항목이 이전에 말한 바는
그것이 서 있던 자리에 인용해 두었다. 이 절이 존재하는 이유는 "## 앞의 두 차수가 틀린
것"이 존재하는 이유와 같다: 각각의 오류가 이 검토가 다루는 결함 부류의 사례이고,
그중 하나를 조용히 지우는 것이 두 번째 감사를 필요하게 만든 바로 그 실수다.

형태가 정확히 되풀이됐다. 앞의 두 차수는 종결이 아닌 것을 **종결**로 표시했다. 3차는
종결된 것을 **미해결**로 표시했다. 둘은 같은 오류다 — 코드를 다시 읽지 않고 앞으로
복사된 처리 — 그리고 두 번째가 무해한 방향인 것도 아니다. 죽은 항목을 나르는 백로그는
읽히기를 그만두고, 그 안의 살아 있는 항목도 함께 딸려 나간다. 이 차수가 시작될 때 이
저장소의 문서 여섯 중 다섯이 코드가 이미 닫은 무언가를 주장하고 있었다.

| # | 3차가 말한 것 | 코드가 말하는 것 | 근거 |
|---|---|---|---|
| 1 | "여전히 미해결" #3: **범위 집계 원장 질의가 없어서** `/global/spend/report`가 호출할 것이 없다 | 존재하고 연결돼 있다 | `store.RollupQuery`와 `Store.ReadRollupRange`(`internal/store/rollup.go`). 어댑터는 `(*adminLedger).Report`(`internal/app/admin.go`)이고, 그 자신의 주석이 *"It answered 501 until now"*라고 말한다. `TestGlobalSpendReportReadsTheRollups`가 못 박는다 |
| 2 | 같은 항목의 후반부: 세 개의 일일 활동 엔드포인트는 **호출할 것이 없다** | **결과는 맞고 이유는 틀렸다** | 여전히 거부하지만, 무엇이 빠졌는지를 이름으로 대는 *살아 있는* 원장에서 거부한다 — `internal/app/admin.go`의 `unsupportedLedger`는 원장에 사용자별 인덱스가 없다고 말하고, `rollupPlan`은 §9.4가 큐브가 아니라 목적에 맞춰 만든 롤업을 구체화하기 때문에 `day`가 아닌 차원 둘을 동시에 요구하는 것을 거부한다. 그것은 대안을 이름으로 댄 의도적 거부이지 의존물 부재가 아니다. "호출할 것이 없다"는 거짓이고, "그것을 답할 큐브가 없다"는 참이며, OPERATIONS §3.2가 이제 말하는 것이 그것이다 |
| 3 | "여전히 미해결" #7: **`internal/luaext`**는 도달 불가, "1차에서 변동 없음" | 도달 가능하며, 요청 경로 위에 있다 | `go.mod`는 `a0d5871` 이래 `github.com/yuin/gopher-lua v1.1.2`를 요구한다. `internal/app/extensions.go`가 설정으로부터 엔진을 만들고 `internal/app/filter.go`가 선언된 모든 `filters.plugins[]` 항목을 컴파일한다. `94de856`, `bb67140`, `241f73b`가 샌드박스를 하드닝했다 — 이 문서가 뼈대라고 부른 서브시스템에 대한 커밋 세 개 분량의 작업이다 |
| 4 | 같은 항목: **`quota.Ranker`**는 도달 불가 | 프로덕션 경로에서 생성된다 | `internal/app/build.go`가 `quota.NewRanker`를 호출한다. 그 연결은 `TestUrgencySourceIsTheQuotaRanker`(`internal/router/wiring_test.go`)가 못 박는다. "### 3. 호출자가 없는 서브시스템 통째"에서 그 곁에 이름이 오른 형제 `Meter.Allowance`는 `internal/metrics/collect_quota.go`가 읽는다 |
| 5 | 같은 항목: **OAuth 서브시스템**은 도달 불가이며, "OAuth 크리덴셜을 설정할 수 없으므로 §11.2b는 바이너리에 없는 기능을 서술한다" | 설정되고, 만들어지고, 시작되고, 닫힌다 | `internal/app/oauthcred.go`의 `buildOAuth`가 모든 `auth: oauth` 크리덴셜을 갱신되는 것으로 바꾼다. `internal/app/app.go`가 그것을 호출하고 갱신 루프를 시작하고 닫는다. 그 자신의 doc 주석이 이것이 §17.1의 결함 부류였고 두 절반이 함께 들어와야 했다고 기록한다 |
| 6 | "여전히 미해결" #5: **업스트림 오버플로 분류기 사본 둘**, 그것들을 맞춰 두는 것이 없음 | 인자를 섞는 수준으로 좁혀졌다 | 둘 다 단일한 `router.ClassifyBody`에 위임한다. 거울은 여전히 거울이므로 위에서 삭제하지 않고 강등했다 |
| 7 | "## 다루지 않은 것", LOW: **인증 전에 라우트 테이블 열거 가능** | 종결됨, 바로 이 문서 안에서 | 세 헤딩 위의 "## `internal/admin` — 마운트됨" 절이 그것을 진술하고 `TestTheRouteTableIsNotReadableBeforeAuthentication`을 이름으로 댄다. 한 절에서 결함을 종결하고 다른 절에서 그것을 미해결로 실어 나르는 문서는, 이 차수가 두 번 찾아낸 파일 간 모순의 단일 파일 판본이다 |
| 8 | "### 3. 호출자가 없는 서브시스템 통째": **`internal/backend`** — 자기 밖에 importer 없음 | 백엔드 계층이 곧 요청 경로다 | 역사적 기록이고, 쓰일 당시에는 참이었다. 기록을 위해 남긴다. §17.1의 "Extracting the backend layer"가 그것을 닫은 변경이다 |

**그 절에서 여전히 참인 것**, 그대로 나른 것이 아니라 재검증한 것: `internal/probe`는
importer가 0개다(`grep`은 주석 둘을 반환하고 import는 없다). `store.ImportKeys`에는
테스트 아닌 호출자가 없다. 그리고 `quota.Budget`은 내구성 있는 `cluster.Ledger`에
의해 의도적으로 대체됐다(DESIGN §18 W9).

**다음번에 쓸 방법 하나.** 위의 모든 거짓 항목은 하나의 표식을 공유한다: 처리가
심볼과 호출 지점이 아니라 *패키지*를 이름으로 댄다. "`internal/luaext` — 뼈대,
importer 없음"은 `internal/luaext`를 읽어서는 반증할 수 없다. 그것은 트리의 나머지에
대한 주장이며, 그것을 결판내는 유일한 것은 그 주장이 제기된 커밋에서 돌린, 패키지
밖의 import에 대한 `git grep`이다. 그것은 이 페이지의 첫 정정이 종결에 대해 진술하는
것과 같은 규칙 — *파일이나 테스트를 이름으로 대는 처리는 그 파일이나 테스트가 그것이
주장된 브랜치 위에 존재해야만 종결이다* — 을 반대 방향으로 적용한 것이다.
**미해결** 처리도 **종결** 처리와 같은 근거를 필요로 한다.
