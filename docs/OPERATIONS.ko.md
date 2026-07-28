# 운영

> 런북: 설치, 기동, 키 발급, 백엔드 추가, 메트릭 읽기, 알람 대응, 백업, 업그레이드, 드레인, 그리고
> 실제로 일어날 실패 모드들.
>
> 이 문서는 **만들어진 시스템**을 서술한다. 설계가 명세하지만 이 빌드에 없는 동작은
> ⚠️ **이 빌드에 없음**으로 표시하고 §12에 이름으로 열거한다. 설정 키는
> [CONFIG.ko.md](CONFIG.ko.md)에 있으며 여기서 반복하지 않는다.
>
> **2026-07-28 작업 트리에 대해 검증됨.** 구현은 움직이고 있고 §0의 서빙 표면은 마일스톤마다 자라 왔다 —
> 이 페이지가 아니라 실제로 돌리는 빌드에 대조할 것. **부재 주장이 가장 먼저 만료되는 주장이다.**
>
> English (기준 문서): [OPERATIONS.md](OPERATIONS.md)

---

## 0. 이 빌드가 서빙하는 것

계획 전에 표면을 알 것. "서빙" 목록에 없는 모든 것은 조용한 `404`가 아니라 **기계 판독 가능한 코드와 함께
501**로 답한다.

### 0.1 서빙

**T0 — 이것들이 없으면 붙어 있는 클라이언트가 즉시 깨진다:**

| 경로 | 메서드 | 인증 |
|---|---|---|
| `/v1/chat/completions`, `/chat/completions` | POST | 필요 |
| `/v1/embeddings`, `/embeddings` | POST | 필요 |
| `/v1/messages`, `/v1/messages/count_tokens` | POST | 필요 |
| `/v1/models`, `/models` | GET, HEAD | 필요 |
| `/health`, `/health/liveness`, `/health/liveliness`, `/health/readiness` | GET, HEAD, OPTIONS | **없음** |
| `/metrics` | GET, HEAD | **없음** |

**T1 — 이것들이 없으면 범용 SDK 호출이 실패한다:**

| 경로 | 메서드 | 비고 |
|---|---|---|
| `/v1/completions`, `/completions` | POST | 레거시 텍스트 completions |
| `/v1/rerank`, `/rerank`, `/v2/rerank` | POST | 배포된 세 철자, 하나의 프로토콜 |
| `/v1/moderations`, `/moderations` | POST | |
| `/v1/audio/speech` | POST | JSON 입력, 바이트 출력 |
| `/v1/audio/transcriptions`, `/v1/audio/translations` | POST | **multipart** |
| `/v1/images/generations` | POST | |
| `/v1/images/edits`, `/v1/images/variations` | POST | **multipart** |
| `/v1/responses` | POST | 그리고 `/v1/responses/{id}`, `/{id}/input_items`, `/{id}/cancel` — 상태를 갖는 하위 리소스는 response store가 있을 때만 마운트된다 |
| `/v1/models/{id}`, `/models/{id}` | GET, HEAD | |
| `/v1/batches`, `/v1/batches/{id}`, `/v1/batches/{id}/cancel` | GET, POST | |
| `/v1/files`, `/v1/files/{id}`, `/v1/files/{id}/content` | GET, POST, DELETE | |

**경로에 배포명이 들어가는 alias** — Azure 형태와 레거시 engine 형태 클라이언트용:
`/engines/{model}/{chat/completions,completions,embeddings}`와 `/openai/deployments/{model}/…`
(후자는 `audio/speech`, `audio/transcriptions`, `audio/translations`, `images/generations`,
`images/edits`, `responses`까지 추가로 덮는다). 이것들은 라우트 표가 **구체성 순서**이기 때문에만
동작한다; 단순 프리픽스 라우터는 그것들을 전부 설정된 패스스루 catch-all에 삼키고, 증상은 Azure 형태
클라이언트가 조용히 벤더 패스스루에 도달하는 것이다.

**패스스루**: `passthrough.routes[]` 항목별 `<prefix>/…`, 라우트별 인증.

우연이 아니라 의도인 등록 세부 둘:

- `/chat/completions`는 `/v1/chat/completions`로 리다이렉트하지 않고 **나란히** 있다. 두 base URL 관례가
  모두 현실에 존재하고 둘 다 동작해야 한다; POST에 대한 `307`은 왕복 하나를 더 쓰고 어떤 클라이언트는
  본문을 떨군다.
- `/health/liveliness`는 dorang이 고른 오타가 아니다. 이 표면이 상호운용해야 하는 널리 배포된 프록시가
  그렇게 쓰고, 현장의 컨테이너 매니페스트가 정확히 그 경로를 프로브하며, 올바른 철자가 나란히 존재한다.

### 0.2 서빙하지 않음 — 사유가 담긴 501

| 코드 | 의미 |
|---|---|
| `route_not_implemented` | dorang이 선언했으나 이 빌드가 마운트하지 않은 라우트: `/v1/ocr`, `/v1/vector_stores`, `/v1/assistants`. `/v1/responses`, `/v1/files`, `/v1/batches`도 이 목록에 있는데, 그 서브시스템이 조건부로 마운트되기 때문이다 — 그것들이 없는 빌드는 "그런 경로 없음"이 아니라 "선언됨, 미구축"으로 답해야 한다 |
| `route_unknown` | 그 밖의 모든 것 |

501은 `X-Dorang-Unimplemented: <path>`를 싣는다. 알려진 경로에 잘못된 메서드는 `Allow` 헤더와 코드
`method_not_allowed`를 담은 **405**로 답한다.

**HTTP 관리 표면은 마운트되어 있다.** `/key/*`, `/user/*`, `/team/*`, `/model/*`, `/budget/*`,
`/spend/*`, `/admin/*`와 `/ui`의 읽기 전용 내장 UI를 `cmd/dorang`이 서빙한다. 접근 조건은
`DORANG_MASTER_KEY` 또는 소유 사용자가 관리 role을 가진 키다. 모든 변경은 `audit_logs` 행을 남긴다.

다만 모든 엔드포인트에 저장소가 붙어 있지는 않다. 붙어 있지 않은 것은 무엇이 없는지를 이름으로 밝히는
**501 `dependency_not_configured`**로 답한다 — 목록은 §3.2, 이유는 §12. 501을 읽을 때 이 구분이
중요하다: `dependency_not_configured`는 *이 빌드가 서빙할 수 없다*, `route_not_implemented`는
*dorang이 아직 만들지 않았다*는 뜻이다.

### 0.3 로그를 읽기 전에 알아 둘 동작 하나

**키의 허용목록 밖 모델은 `403`이 아니라 `401`을 답한다.** 호출자는 정상적으로 인증됐으므로 `403`이 더
그럴듯하지만 — 배포된 클라이언트들이 만들어진 대상 참조 프록시가 여기서 `401`을 답하고 클라이언트가 그것으로
분기한다. 에러 분류표가 함의하는 것으로부터의 의도적 divergence다; [COMPATIBILITY.md](COMPATIBILITY.md)
§7.2와 §11.2 참조.

서빙되는 모든 것의 바이트 수준 계약은 [COMPATIBILITY.ko.md](COMPATIBILITY.ko.md)에 있고, 거기의 모든
행은 참고사항이 아니라 골든 테스트다.

---

## 1. 설치

### 1.1 바이너리

릴리스 아카이브는 `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`용 두 바이너리를 각각
`.sha256`과 함께 담는다.

```
tar xzf dorang_v1.2.3_linux_amd64.tar.gz
sha256sum -c dorang_v1.2.3_linux_amd64.tar.gz.sha256
install -m 0755 dorang dorangctl /usr/local/bin/
dorang --version
```

전 구간 `CGO_ENABLED=0`: SQLite 드라이버가 순수 Go라 결과가 libc 의존 없는 정적 바이너리다. 그것이 노트북
티어의 "필수 의존성 0"을 문자 그대로 참으로 만들고, 릴리스 바이너리가 해당 아키텍처의 어떤 커널에서도
도는 이유다.

소스에서:

```
make build          # -> bin/dorang, bin/dorangctl
make test
make test-race
```

### 1.2 컨테이너

```
docker pull ghcr.io/ziozzang/dorang:1.2.3
```

런타임 레이어는 `gcr.io/distroless/static-debian12:nonroot`다 — 두 바이너리와 TLS 루트 저장소뿐,
쉘 없음, 패키지 매니저 없음, 프로세스가 손상돼도 피벗할 것이 없음. `nonroot`로 돌고 `4100`을 노출하며
`/var/lib/dorang`에 볼륨을 선언한다.

엔트리포인트는 `dorang --config /etc/dorang/config.yaml`이다.

이미지에 대해 물릴 두 가지:

⚠️ **`HEALTHCHECK`가 깨져 있다.** `dorangctl health --addr http://127.0.0.1:4100`을 호출하는데
`dorangctl`에는 **`health` 서브커맨드가 없다** — 호출이 `unknown command "health"`와 함께 2로 종료하므로,
프로세스의 실제 상태와 무관하게 `start-period + 3 × interval` 이후로 컨테이너가 unhealthy로 보고된다.
override하거나, 없애고 오케스트레이터 프로브를 쓸 것. distroless 이미지에는 `wget`도 `curl`도 없으므로
현실적으로는 `--no-healthcheck`로 돌리고 컨테이너 밖에서 `/health/readiness`를 프로브한다.

⚠️ **`DORANG_STATE_DIR=/var/lib/dorang`은 이미지가 설정하고 아무것도 읽지 않는다.** state 경로는
`storage.sqlite.path`와 `metering.spool.dir`에서 오고 기본값이 `~/.dorang/…`인데, `nonroot`에서 그것은
선언된 볼륨이 **아니라** `/home/nonroot`다. 명시적으로 볼륨을 가리킬 것:

```yaml
storage: {driver: sqlite, sqlite: {path: /var/lib/dorang/dorang.db}}
metering: {spool: {dir: /var/lib/dorang/spool}}
```

아니면 데이터베이스와 spool이 컨테이너 쓰기 레이어에 살다가 `docker rm`에서 사라진다.

### 1.3 Kubernetes 프로브

```yaml
livenessProbe:
  httpGet: {path: /health/liveness, port: 4100}
  periodSeconds: 10
readinessProbe:
  httpGet: {path: /health/readiness, port: 4100}
  periodSeconds: 2
```

**둘 다** 쓰고, liveness를 readiness에 겨누지 말 것. 드레인 중인 노드는 살아 있으며 재시작되면 안 된다;
둘을 혼동하면 graceful drain이 kill이 된다. liveness는 드레인 내내 `200 {"status":"alive"}`를 유지하고,
readiness는 드레인이 시작되는 순간 `503 {"status":"draining"}`으로 뒤집힌다.

`terminationGracePeriodSeconds`는 `server.shutdown_grace`에 드레인 이후 해체(같은 값으로 제한)를 더한 것보다
커야 한다. `2 × shutdown_grace + 5s`로 잡을 것.

---

## 2. 첫 기동

### 2.1 설정 없는 기동

`--config`도 `$DORANG_CONFIG`도 작업 디렉터리의 `dorang.yaml`도 없으면, 서버는 문서화된 기본값으로 뜬다:
`~/.dorang/dorang.db`의 SQLite, 업스트림 없음, `:4100` 리슨. `/health*`, `/metrics`, (비어 있는)
`/v1/models`를 서빙하고 나머지는 501이다 — 그것이 "필수 의존성 없는 단일 바이너리가 요청을 서빙한다"는
말의 뜻이다. 명시적으로 지정한 파일이 없으면 에러다. 선택이 아니라 오타이기 때문이다.

### 2.2 정상 기동

```
export DORANG_MASTER_KEY="$(head -c 32 /dev/urandom | base64)"
export DORANG_KEY_PEPPER="$(head -c 32 /dev/urandom | base64)"  # generated, not a literal
export CLOUD_A_KEY_1=…

dorangctl config lint /etc/dorang/config.yaml
dorang --config /etc/dorang/config.yaml --check
dorangctl migrate --config /etc/dorang/config.yaml
dorang --config /etc/dorang/config.yaml
```

성공 시:

```
dorang v1.2.3 listening on [::]:4100 (/etc/dorang/config.yaml)
```

실패 시 모든 문제가 YAML 경로와 함께 한 번에 출력된다:

```
dorang: /etc/dorang/config.yaml: refusing to start, 2 problem(s):
  cluster.capacity_mode: cluster.enabled is true with capacity_mode "local", which refuses to start: …
  credentials[1].key_env: environment variable CLOUD_A_KEY_2 is not set
```

### 2.3 무엇이든 발급하기 전에 pepper를 설정할 것

`DORANG_KEY_PEPPER`가 미설정이면 pepper가 **한 번 생성되어 SQLite 데이터베이스 옆에 기록**되고, 그래서
노트북 티어가 아무것도 설치하지 않고 동작한다. 그 결과:

- pepper가 데이터베이스 파일과 함께 다닌다. 함께 백업하지 않으면 발급한 모든 키가 검증 불가능해진다.
- 프로세스가 둘 이상이면 각자 자기 pepper를 만들고, 한 노드가 발급한 키가 다른 모든 노드에서 인증에
  실패한다. **프로세스가 둘 이상인 배포에서는 변수를 명시적으로 설정할 것.** 서버는 pepper를 생성했을 때
  로그를 남긴다.

### 2.4 마이그레이션

`dorangctl migrate`는 내장 마이그레이션을 적용하고 종료하며 적용한 것을 보고한다:

```
applied 3 migration(s), 0 -> 3:
  1 initial
  2 batch_columns
  3 notional_column
```

또는 `schema is already at version 3; nothing to apply`. 서버도 기동 시 마이그레이션을 적용하므로, 명시적
호출은 스키마 변경을 별도의 검토 가능한 단계로 두고 싶은 단계적 배포를 위한 것이다.

### 2.5 확인

```
curl -s localhost:4100/health/readiness            # {"status":"ready"}
curl -s localhost:4100/metrics | head
curl -s localhost:4100/v1/models -H 'Authorization: Bearer sk-…'
```

---

## 3. 키 발급

키는 `sk-` 프리픽스 + 32바이트 난수, 패딩 없는 base64url이다. 저장되는 것은
`HMAC-SHA256(pepper, token)`이라 데이터베이스를 훔쳐도 오프라인 공격이 불가능하다. 복구 가능한 것은 아무것도
보관되지 않으며, 그래서 토큰이 한 번만 출력된다.

```
dorangctl key create --config /etc/dorang/config.yaml \
  --alias "team-search" \
  --user u_42 --team t_7 \
  --models model-large,model-small \
  --routes /v1/chat/completions,/v1/models \
  --budget-usd 250 \
  --ttl 2160h \
  --priority-class interactive
```

| 플래그 | 의미 |
|---|---|
| `--alias` | 표시용 별칭 |
| `--user`, `--team` | 소유 id |
| `--models` | 쉼표 구분 허용목록; 비면 전체 허용. 이름은 **통째로** 비교 |
| `--routes` | 쉼표 구분 라우트 허용목록; 비면 전체 허용 |
| `--budget-usd` | 지출 상한. `0`은 상한 없음 |
| `--rpm`, `--tpm` | rate 상한. ⚠️ 이 빌드에서 저장되고 **강제되지 않는다** — §12 |
| `--max-parallel` | 동시성 상한. ⚠️ 저장되고 **강제되지 않는다**; 대신 `capacity.principals.<key-id>`를 쓸 것 |
| `--ttl` | 이만큼 뒤 만료. `0`은 만료 없음 |
| `--priority-class` | priority 클래스 이름 |

토큰은 **stdout**으로, 확인 메시지는 stderr로 가므로 `dorangctl key create … > key.txt`가 정확히 토큰만
캡처한다.

```
dorangctl key list --config /etc/dorang/config.yaml --limit 50
dorangctl key revoke --config /etc/dorang/config.yaml <key-id>
```

`key list`는 id, label, alias, user, team, 해시 스킴, 상태(`active`/`blocked`/`expired`), 지출, 예산,
출처, 생성 시각을 보여준다.

**revoke는 차단이지 삭제가 아니다.** 원장이 키 id를 참조하므로 행을 삭제하면 과거 요청이 전부 고아가 된다.
만료되거나 차단된 키는 남아 있으면서 *그렇게* 거부되어야 하지, 사라져서 미지의 키로 거부되면 안 된다.

⚠️ **동작 중인 게이트웨이는 크리덴셜을 캐시한다.** 폐기는 즉시가 아니라 캐시 엔트리 TTL 이내 또는 다음
리로드에 반영된다. `dorangctl key revoke`가 stderr로 그렇게 말한다. 지금 필요하면 폐기 후 프로세스에
`SIGHUP`을 보낼 것.

### 3.1 유출된 키 폐기

사고 대응 절차다. HTTP 클라이언트와 관리 크리덴셜만 있으면 된다 — 서버 셸도, DB 클라이언트도, 재시작도
필요 없다.

```
curl -s -X POST http://gateway:4000/key/block \
  -H "Authorization: Bearer $DORANG_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"key_id":"<key id>"}'
```

응답은 키의 새 상태다. 호출이 반환되기 전에 `audit_logs` 행이 기록된다. 감사 기록을 남길 수 없는 변경은
조용히 적용되지 않고 거부되며, 변경이 적용된 *뒤에* 기록에 실패하면 재시도가 아니라 대조하라는 뜻의
`500 audit_write_failed`가 돌아온다.

**언제 반영되는가.** `internal/auth`는 로드된 크리덴셜을 `DefaultEntryTTL`(60초) 동안 캐시하므로 차단
후 1분 이내에 키가 동작을 멈춘다. `Authenticator.use`는 인가 검사와 독립적으로 매 요청 `Blocked`를
강제하므로 두 번째 관문에 도달할 필요가 없다. 그 1분이 중요하면 `SIGHUP`으로 캐시를 즉시 비운다.

**진행 중인 배치도 멈춘다.** `batchExecutor`는 30초 소유자 해석 TTL 안에서 소유 크리덴셜을 행마다 다시
해석하고 `Authorize`를 호출하므로, 차단된 키의 실행 중 배치는 지출을 멈춘다. 이 검사가 없던 시절에는
차단 전에 제출된 배치가 끝날 때까지 계속 지출했다.

**유출 신고에서 키 id 찾기.** 라벨은 lookup 다이제스트의 앞 8자리 hex에서 유도되며(`store.LabelFor`)
비밀 자체의 문자에서 유도되지 않는다. 따라서 유출된 토큰을 어디에도 보내지 않고 라벨만으로 행을 특정할
수 있다:

```
curl -s "http://gateway:4000/key/list?limit=200" \
  -H "Authorization: Bearer $DORANG_MASTER_KEY"
```

⚠️ **평문 키를 파라미터로 보내지 말 것.** `/key/info`와 `/key/block`은 `key_id`를 받고 `key`
파라미터는 코드 `secret_in_request`로 거부한다. URL 안의 살아 있는 크리덴셜은 액세스 로그, 프록시 추적,
셸 히스토리 안의 살아 있는 크리덴셜이다 — 지금 대응 중인 그 유출이 흔히 그렇게 생긴다.

**차단 대신 회전.** `POST /key/regenerate`는 같은 키 id 뒤의 비밀만 새로 발급하고 모든 인가 필드를
유지하며, 새 토큰을 정확히 한 번 반환한다. 전환은 즉시다: 옛 비밀이 계속 검증되는 유예 창은 없다. 사고
경로에서의 유예 창은 침해된 비밀을 그 기간만큼 계속 살려 두는 것이기 때문이다.

**이 표면이 마운트되기 전에는** 유일한 수단이 데이터베이스에 직접 쓰는
`UPDATE api_keys SET blocked = 1 WHERE id = '<key id>'`였다. 노드 셸 접근이 필요하고, 런북에서 HTTP로
실행할 수 없으며, 감사 행도 남지 않는다. 이제 그것은 절차가 아니다. 관리 크리덴셜 자체가 유출된 경우의
비상 수단으로만 남는다.

### 3.2 관리 표면이 서빙하는 것과 서빙하지 않는 것

마운트되어 동작하는 것:

| 경로 | 비고 |
|---|---|
| `/key/generate`, `/key/info`, `/key/update`, `/key/delete`, `/key/list`, `/key/block`, `/key/unblock`, `/key/regenerate` | 크리덴셜 수명주기 전체. `/key/regenerate`는 유예 없이 옛 시크릿을 즉시 끊는다 — 사고 대응 경로다 |
| `/key/rotate`, `/key/rotate/cut`, `/key/secrets` | DESIGN §11.2c 로테이션: 유예 기간을 둔 새 시크릿, 조기 컷, "클라이언트가 갈아탔는가"를 답하기 위한 목록. 키 id는 그대로이므로 예산·지출·허용 목록·원장 이력은 건드리지 않는다 |
| `/key/pend`, `/key/release` | §11.6의 되돌릴 수 있는 거부. block과 구분된다 — pend는 틀릴 수 있는 통계적 판단이고 운영자가 한 동작으로 해제한다 |
| `/spend/logs` | 요청별 원장. `key_id`, `team_id`, `trace_id`, `tag`, `errors_only` 중 하나와 유계 날짜 범위가 필요하다 — §9.3은 무계 스캔을 느리게 답하지 않고 거부한다 |
| `/admin/capacity` | 축별 브로커 점유 현황 |
| `/admin/catalog/explain`, `/admin/catalog/unverified` | 카탈로그 모델 필드의 출처 |
| `/health/history` | 인프로세스 링. 재시작할 때마다 비어 있는 상태로 시작한다 |
| `/ui` | 읽기 전용 운영 UI. 관리 크리덴셜로 로그인하며 세션은 1시간이다 — 폐기된 크리덴셜이 UI를 잃기까지의 지연이기도 하다 |

이 빌드에서 **501 `dependency_not_configured`**로 답하는 것. 뒤에 있어야 할 저장소가 아직 없기
때문이다 — `internal/store`에는 `users`, `teams`, `team_members`, `deployments`, `model_aliases`에
대한 Go 코드가 없고 범위 집계 리포트 쿼리도 없다:

| 경로 | 없는 것 | 대신 사용 |
|---|---|---|
| `/user/*`, `/team/*` | 디렉터리 | `dorangctl`, 또는 데이터베이스 |
| `/model/*`, `/model_group/info` | 모델 레지스트리 | 설정 + `SIGHUP` |
| `/budget/*` | 예산 저장소 | `dorangctl key create --budget-usd` |
| `/global/spend/report`, `/user/daily/activity`, `/team/daily/activity`, `/tag/daily/activity` | 롤업 쿼리 | `/spend/logs`, 또는 `request_logs` 직접 조회 |
| `/admin/credentials/health`, `/admin/quota` | 쿼터 레지스트리 | `/metrics` |
| `/spend/calculate`, `/admin/pricing/preview` | 가격 엔진 | — |
| `/admin/config/reload` | 리로더 | `SIGHUP` |

### 3.3 관리 범위(scope)

관리자는 두 종류이며, 그 차이는 스키마가 이미 가지고 있는 사실이다.

- **전역.** `DORANG_MASTER_KEY`, 그리고 **어느 팀에도 속하지 않은** 관리 키. 운영자다: 모든 주체, 모든
  엔드포인트.
- **팀 범위.** 팀에 **속한**(`api_keys.team_id`) 관리 키. 그 팀의 키, 멤버, 예산, 지출만 관리하며 그
  밖은 아무것도 관리하지 못한다.

범위는 저장되지 않고 키에서 유도되므로 잊을 수가 없다: 팀에 속한 키는 속해 있다는 사실만으로 범위가
정해진다. 설정해야 할 `admin_team` role도, 빠뜨릴 마이그레이션도 없다.

팀 범위 관리자가 받는 답:

| 시도 | 답 |
|---|---|
| 자기 팀 키를 id로 조회 | 서빙 |
| 다른 팀 키를 id로 조회 | **404**, 존재하지 않는 id와 동일 — 403이면 모든 키 id가 존재 여부 오라클이 된다 |
| `?team_id=`로 다른 팀 지정 | **403 `out_of_scope`** — 파라미터 거부는 아무것도 노출하지 않으면서 어느 파라미터를 고칠지 알려 준다 |
| 필터 없는 `/key/list` | 자기 팀만. 배포 전체가 아니다 |
| `key_id`/`trace_id`로 다른 팀 원장 행에 도달 | 행이 제거된다. 저장소 필터는 최적화이고 행 검사가 강제다 |
| 다른 팀에, 또는 어느 팀에도 속하지 않게 키 발급 | **403** — 팀 없는 키는 *전역* 관리자가 된다 |
| 사용자의 `user_role` 변경 | **403** — role이 누가 관리자인지를 결정하므로 그것을 쓰는 것은 권한 상승이다 |
| `/model/*`, `/admin/*`, `/global/spend/report`, `/user/new`, `/team/new`, `/health/history` | **403** — 배포 전역이며 "설정을 리로드한다"의 팀별 뷰라는 것은 없다 |

인증은 됐지만 관리 권한이 없는 크리덴셜은 401이 아니라 **403**을 받는다. 동작하는 키에게 키가 동작하지
않는다고 말하면 운영자를 엉뚱한 문제로 보내기 때문이다. **401**은 쓸 수 있는 크리덴셜이 없다는 뜻이고,
이제 인증되지 않은 호출자에게는 알려지지 않은 경로를 포함해 모든 관리 경로가 401로 답한다. 인증 전에
라우트 테이블을 읽을 수 없다.

### 3.4 관리 크리덴셜

`DORANG_MASTER_KEY`는 out-of-band로 상수 시간 비교되며 **절대 행이 아니다.** 발급되지도, 목록에
나오지도, `dorangctl`로 폐기되지도 않는다. 회전은 환경 변경 + 재시작이다.

authenticator는 명시적 opt-out이 없는 한 그것 없이 구성을 거부한다. 의도적이다: 임포트된 크리덴셜
데이터베이스만 읽는 게이트웨이는 관리자가 아예 없고 그것을 알아채지 못한다.

---

## 4. 프로바이더와 모델 추가

모든 참조가 로드 시 이름으로 검사되므로 순서가 중요하다.

**1. 프로바이더 선언.** **2. capacity 그룹 선언** — 아니면 프로바이더 참조가 거부된다. **3. 크리덴셜
선언** — 비밀값은 참조로, 환경 변수는 실제로 설정.

**4. 라우팅하기 전에 카탈로그가 그 모델을 어떻게 아는지 확인:**

```
dorangctl catalog explain anthropic my-model-name
dorangctl catalog unverified
```

`catalog explain`은 필드별로 해결된 값, 어느 레이어가 이겼는지, 어느 파일이 썼는지를 출력한다. 컨텍스트
윈도우가 틀린 오퍼레이터는 어느 파일을 고칠지 알아야 하고, 오버레이를 쓴 오퍼레이터는 그것이 적용됐는지
알아야 한다. 이것 없이는 둘 다 추측이다.

`catalog unverified`는 프로브 목록이다: dorang이 검증 날짜 없는 데이터로 능력 질문에 답하게 될 모델들.
**미검증 능력은 부재 능력과 같지 않으며**, 특히 리즈닝 능력은 이름 프리픽스에서 절대 추론되지 않는다 —
한 모델 버전에서 관측한 effort 스케일을 패밀리 전체로 일반화하는 것이 그 패밀리의 다른 멤버들에서 제어를
조용히 떨어뜨리는 정확한 경로다.

**5. 배포 추가.** **6. 가격 매기기** — 아니면 무료로 라우팅된다. 가격 없는 모델은 라우팅에서 *의견 없음*
이지 가장 싼 것이 아니지만, 동시에 0원이 아니라 UNPRICED로 기록된다:

```
dorangctl price model-x --config /etc/dorang/config.yaml --input 12000 --output 800 --cached-read 9000
```

`price`와 `catalog explain`은 대상을 **먼저**, 플래그를 뒤에 받는다; `key create`, `key list`,
`migrate`는 플래그만 받는다. 미관 문제가 아니다 — Go의 flag 패키지가 첫 비플래그 토큰에서 멈추므로
`dorangctl price --config … model-x --input 10`은 아무것도 파싱하지 않는다.

클래스별 적용 규칙 체인, 각 컴포넌트의 요율과 수량, 소계, 최종 금액, **각 규칙이 왜 선택됐는지**, 그리고
출처와 나이가 붙은 notional 정가 등가를 출력한다. 원장이 돌리는 같은 평가기를 돌리므로 여기 숫자, 계산기가
보여줄 숫자, 원장의 숫자가 같은 숫자다.

`no marginal_usage rule matched` 경고가 나오면 그 요청은 UNPRICED로 기록된다. 배포가 트래픽을 싣기 전에
고칠 것.

**7. 린트하고 리로드.**

```
dorangctl config lint /etc/dorang/config.yaml
kill -HUP $(pidof dorang)
```

실패한 리로드는 돌던 설정을 유지하고
`reload refused, keeping the running configuration: …`를 로그한다. 부분 적용은 없다.

리로드가 재구성하지 **않는** 것을 기억할 것: 스토어, authenticator, meter, capacity broker.
`storage.*`나 capacity 상한 변경에는 재시작이 필요하다.

---

## 5. self-hosted 백엔드

dorang은 엔진을 튜닝하지 않고, 빠진 플래그를 게이트웨이가 흉내 내 덮지 않는다. prompt 토큰 상세가 켜져
있지 않으면 캐시 토큰 가격을 추측하는 대신 사용 불가로 보고한다 — 흉내는 게이트웨이의 숫자를 엔진의 숫자와
어긋나게 만들고, 어긋나는 숫자는 없는 숫자보다 나쁘기 때문이다.

그래서 플래그는 오퍼레이터의 일이고, 두 엔진 모두 빠졌을 때 **요청을 받아들이고 요청받은 것을 무시한 채
`200`을 반환한다.** 없을 때 조용히 깨지는 것과 함께 전체 프로필:

- **vLLM** — [VLLM.ko.md](VLLM.ko.md) §5, 그리고 §1.2(기본 스케줄러 정책에서 priority가 조용히 무시되며
  필드 설명 자체가 반대로 말한다), §3(부하 신호와 그 네 함정), §8(vLLM 문서가 소스와 어긋나는 다섯 곳).
- **SGLang** — [SGLANG.ko.md](SGLANG.ko.md) §8.1(vLLM과 나란한 공통 런북), §8.2(설정할 것),
  §8.3(**설정하면 안 되는** 것), §8.4(보안 자세 — SGLang 포트를 노출하기 전에 읽을 것).

오늘 오퍼레이터의 행동을 바꾸는 세 항목, 조용한 실패이므로 여기서 반복한다:

| 증상 | 원인 | 조치 |
|---|---|---|
| 모든 캐시 요청이 정가로 청구된다 | vLLM: `--enable-prompt-tokens-details` 미설정. SGLang: `--enable-cache-report` 미설정 | 플래그를 설정할 것. dorang은 차이를 탐지할 수 없다; null `cached_tokens`와 진짜 0은 똑같아 보인다 |
| 에이전틱 클라이언트가 툴 호출 대신 평문을 받고 에러가 없다 | SGLang: `--tool-call-parser` 미설정. 툴은 여전히 프롬프트에 렌더링되고 모델의 네이티브 문법이 `message.content`에 착지 | 파서를 설정할 것. vLLM은 이 상황에서 400을 내고 SGLang은 200을 반환한다 |
| `priority`가 효과가 없다 | vLLM: `--scheduling-policy priority` 미설정. SGLang: `--enable-priority-scheduling` 미설정 | 설정할 것. 그리고 두 엔진이 priority를 **반대 방향**으로 정렬한다는 점 — [CONFIG.ko.md](CONFIG.ko.md) §20.1 |

⚠️ **`--allow-auto-truncate`(SGLang)를 설정하거나 `truncation: "auto"` / `truncate_prompt_tokens`(vLLM)를
보내지 말 것.** 둘 다 컨텍스트 초과를 절삭된 프롬프트에 대한 조용한 `200`으로 바꾸며, 그것이
`context_window` 폴백이 가진 유일한 신호를 제거한다.

---

## 6. 메트릭 읽기

`GET /metrics`는 Prometheus 텍스트 exposition 0.0.4(`text/plain; version=0.0.4`)를 반환한다. 클라이언트
라이브러리가 아니라 직접 작성한 것이다 — 노트북 티어에는 필수 의존성이 없고 거기에 메트릭 라이브러리도
포함된다.

⚠️ **`/metrics`는 인증이 없고 `observability.prometheus: false`로 끄지 못한다.** 그 설정 키는 절대 읽히지
않는다. 포트에 닿을 수 있으면 스크레이프에도 닿는다. 네트워크 경계 뒤에 둘 것.

⚠️ **모든 메트릭은 호출자별 라벨이 없다.** 경로별·모델별·키별 라벨이 어디에도 없으며 의도적이다: 축 키와
모델 이름은 무한 카디널리티이고, 클라이언트가 라벨 값을 만들게 하는 게이트웨이는 자기 메트릭 레지스트리에
대한 서비스 거부를 넘겨준 것이다. 키별·모델별 수치는 스크레이프가 아니라 원장에서 온다.

### 6.1 코어 메트릭

| 메트릭 | 타입 | 재는 것 |
|---|---|---|
| `dorang_requests_total` | counter | 서빙된 요청 |
| `dorang_responses_total{class}` | counter | 상태 클래스별 응답: `1xx`…`5xx` |
| `dorang_request_duration_seconds{le}` | histogram | 게이트웨이 요청 소요. 버킷 100 µs–60 s |
| `dorang_request_bytes_total` | counter | 읽은 요청 본문 바이트 |
| `dorang_response_bytes_total` | counter | 쓴 응답 본문 바이트 |
| `dorang_unimplemented_total` | counter | 501로 답한 요청 |
| `dorang_auth_failures_total` | counter | 인증·인가로 거부된 요청 |
| `dorang_late_errors_total` | counter | 응답이 이미 시작돼 **인밴드**로 전달된 에러 |
| `dorang_handler_panics_total` | counter | 핸들러에서 복구된 panic |
| `dorang_meter_panics_total` | counter | meter에서 복구된 panic; 요청에는 영향 없음 |
| `dorang_passthrough_requests_total` | counter | ⚠️ 이 빌드에 증가 지점이 없다; 항상 0 |
| `dorang_websocket_upgrades_total` | counter | 중계된 WebSocket 업그레이드 |
| `dorang_replay_refused_total` | counter | 프로세스 전역 재생 예산이 가득 차 non-replayable로 표시된 요청 |
| `dorang_body_too_large_total` | counter | 본문 상한 초과로 거부된 요청 |
| `dorang_shadow_observed_total` | counter | shadow 비교를 위해 캡처된 요청 |
| `dorang_observer_panics_total` | counter | shadow observer에서 복구된 panic |
| `dorang_inflight_requests` | gauge | 현재 서빙 중인 요청 |
| `dorang_replay_bytes` | gauge | 재생을 위해 보유 중인 요청 본문 바이트 |
| `dorang_ready` | gauge | 새 작업을 받으면 `1`, 드레인 중 `0` |
| `dorang_uptime_seconds` | gauge | 기동 후 경과 초 |

### 6.2 shadow 메트릭

`shadow.mode != off`일 때만 존재한다. 전부 라벨 없음. 전체 목록과 의미는
[MIGRATION.ko.md](MIGRATION.ko.md) §4.4. 알람이 볼 것들:

`dorang_shadow_cost_capped`(gauge, 일일 상한이 shadow를 멈췄으면 `1`),
`dorang_shadow_with_diffs_total`, `dorang_shadow_inconclusive_total`,
`dorang_shadow_dropped_total`, `dorang_shadow_reference_errors_total`,
`dorang_shadow_skipped_unsafe_total`, `dorang_shadow_report_dropped_total`.

> `dorang_shadow_cost_stops_total`은 `_total` 접미사에도 불구하고 **gauge**로 선언돼 있다. 상한 발동
> 횟수의 단조 카운트이니 쿼리에서는 counter로 다루고, 린터가 불평할 것을 예상할 것.

### 6.3 health 엔드포인트

| 경로 | 정상 | 드레인 중 |
|---|---|---|
| `/health/liveness`, `/health/liveliness` | `200 {"status":"alive"}` | `200 {"status":"alive"}` |
| `/health/readiness` | `200 {"status":"ready"}` | **`503 {"status":"draining"}`** |
| `/health` | `200 {"status":"healthy"}` | **`503 {"status":"draining"}`** |

shadow가 켜져 있으면 본문이 게이트 판정을 담은 `"shadow"` 객체를 싣는다:

```json
{"status":"healthy","shadow":{"mode":"compare","sample_rate":0.05,"cost_capped":false,
 "spent_usd":"1.240000000","limit_usd":"5.000000000","sampled":812,"compared":790,
 "clean":790,"with_diffs":0,"inconclusive":0,"queue_dropped":0,"reference_errors":0,
 "report_dropped":0,"skipped_unsafe":22,"gate":"clean"}}
```

멈춘 shadow는 절대 게이트웨이를 unhealthy로 만들지 않는다. 멈춘 shadow는 서빙 실패가 아니라 진단 실패이고,
그것 때문에 pod를 rotation에서 빼는 것은 진단을 장애로 바꾸는 일이다. 그것이 정확히 health 엔드포인트에
나타나는 이유이기도 하다: "컷오버 게이트가 아직 돌고 있는가"는 health 엔드포인트에 묻는 질문이고, 사흘 전에
조용히 멈춘 상한은 그러지 않으면 누군가 빈 diff 리포트를 준비 완료의 증거로 읽을 때 발견된다.

### 6.4 요청별 헤더

모든 응답이 무조건 싣는 것: `X-Dorang-Request-Id`, `X-Dorang-Model`, `X-Dorang-Upstream-Model`,
`X-Dorang-Deployment`, `X-Dorang-Cost-Usd`, 그리고 429에서의 `Retry-After`와 알려진 경우의
`X-Ratelimit-*` 집합.

규칙은 **클라이언트가 행동하는 헤더는 무조건, 읽는 헤더는 게이팅 가능**이다. `Retry-After`를 텔레메트리
플래그 뒤에 두면 모든 SDK의 백오프가 조용히 동작을 멈추는데, 초기 초안이 "헤더 집합을 제한한다"는 규칙을
문자 그대로 적용해 그렇게 했다.

`X-Dorang-Detail: full`을 보내거나 `observability.always_full_headers`를 켜면 추가되는 것:
`X-Dorang-Provider`, `-Credential`, `-Attempt`, `-Fallback-From`, `-Route-Reason`, `-Queue-Ms`,
`-Ttft-Ms`, `-Latency-Ms`, `-Tokens-Input`, `-Tokens-Output`, `-Tokens-Cache-Read`,
`-Tokens-Cache-Write`, `-Tokens-Reasoning`, `-Notional-Usd`, `-Spend-Usd`, `-Budget-Usd`,
`-Budget-Remaining-Usd`, `-Quota-<Window>-Used-Pct`, `-Dropped-Params`, `-Native-Stop-Reason`,
`-Replayable`.

카운터 헤더는 **0일 때 생략된다.** 없는 카운터와 0인 카운터는 다른 주장이기 때문이다.
`X-Dorang-Replayable`은 `false`일 때만 나타나고 그래서 무조건이다 — 호출자가 의존하고 있을 수 있는 재시도
의미론을 바꾸기 때문이다.

`X-Dorang-Native-Error-Type`은 detail 플래그와 무관하게 에러에 붙는다. 업스트림 자신의 에러 `type`과
`code`는 기록되어 거기 노출되며 **응답 본문에는 절대 들어가지 않는다**: 전달하면 `type`으로 분기하는
클라이언트가 벤더별 문자열에 오분기하게 된다.

인바운드 요청 id 헤더 셋이 존중되고 첫 번째 비어 있지 않은 것이 이기며 128바이트로 제한된다:
`X-Dorang-Request-Id`, `X-Request-Id`, `X-Correlation-Id`.

---

## 7. 알람

각 행은 페이징할 가치가 있는 조건, 실제 의미, 첫 조치다.

### 7.1 서빙

| 알람 | 식 | 의미 | 조치 |
|---|---|---|---|
| 게이트웨이 에러 | `rate(dorang_responses_total{class="5xx"}[5m]) > 0` | 게이트웨이 결함이거나 폴백 체인을 살아남은 업스트림 5xx. `502`는 폴백 이후 업스트림, `500`은 dorang 자신의 결함 | 먼저 `dorang_handler_panics_total`을 볼 것. 0이 아니면 capacity 문제가 아니라 버그다 |
| Panic | `increase(dorang_handler_panics_total[15m]) > 0` | 핸들러가 panic했고 복구됐다. 요청은 실패했고 프로세스는 아니다 | 로그 줄과 request id를 확보할 것. 언제나 결함이다 |
| Late error | `rate(dorang_late_errors_total[5m]) > 0` | 응답이 이미 시작돼 에러를 **인밴드**로 전달해야 했다. 클라이언트는 HTTP 200 뒤 에러 이벤트를 봤다 | 업스트림 상태와 상관 확인. 이것이 폴백이 의도적으로 넘지 않는 경계다 — 중복 출력은 가시적 실패보다 나쁘다 |
| Not ready | `dorang_ready == 0`이 `shutdown_grace`보다 오래 | 노드가 드레인 중이거나 드레인이 끝나지 않았다 | 배포가 진행 중이 아니라면 프로세스가 드레인에 걸려 있다. §10 |
| 501 폭주 | `rate(dorang_unimplemented_total[5m])` 상승 | 클라이언트가 이 빌드가 서빙하지 않는 라우트를 호출 중 — 흔히 `/v1/responses`나 관리 경로(§0.2) | 샘플 응답의 `X-Dorang-Unimplemented`를 읽을 것. 경로를 명시한다 |
| 인증 실패 | `rate(dorang_auth_failures_total[5m])` 상승 | 만료된 키, 클라이언트 설정에 남은 폐기된 키, 또는 다른 pepper로 발급된 키 | `dorangctl key list`가 키별 상태를 보여준다. *모든* 키가 실패하면 pepper를 의심할 것(§2.3) |
| 재생 거부 | `rate(dorang_replay_refused_total[5m]) > 0` | 프로세스 전역 재생 예산이 가득 차 본문이 더 이상 보유되지 않고 그 요청들은 폴백할 수 없다 | 큰 본문 + 높은 동시성. 각각 하나만이면 괜찮다. `dorang_replay_bytes`를 볼 것 |
| 본문 거부 | `rate(dorang_body_too_large_total[5m]) > 0` | 본문 상한 초과 요청 | 클라이언트가 전에 보내지 않던 것을 보내고 있다 — 흔히 임베딩된 문서 |

### 7.2 지연

| 알람 | 식 | 의미 | 조치 |
|---|---|---|---|
| 요청 소요 p99 | `histogram_quantile(0.99, rate(dorang_request_duration_seconds_bucket[5m]))` | ⚠️ 업스트림 시간을 포함한다. 설계가 목표로 삼는 **게이트웨이 오버헤드가 아니다** | 이 시리즈에 대해 설계의 2 ms p99로 알람을 걸지 말 것. 자기 트래픽에 대해 측정한 baseline으로 걸 것 |
| 최상위 버킷에 몰림 | 질량이 전부 `le="+Inf"` | 요청이 60초를 넘고 있다. 보통 긴 생성, 가끔 멈춘 업스트림 | `dorang_inflight_requests`와 상관 확인 — 멈춘 업스트림은 슬롯을 붙든다 |

> **공표된 목표에 대해.** 설계의 숫자는 *게이트웨이 오버헤드*다: 요청 라인과 헤더의 마지막 바이트를 읽은
> 시점부터 업스트림에 첫 바이트를 쓸 때까지, 더하기 업스트림 마지막 바이트부터 클라이언트에 마지막 바이트를
> 쓸 때까지이며 **업스트림 시간은 제외**한다. 측정된 구성 요소: 게이트만 59 ns 무할당; HTTP 표면을 통한
> 종단 간 1.5 µs, 할당 17회, warm-local 예산의 0.75%; 후보 10개에 prefix·sticky·capacity·pricing이 모두
> 살아 있는 라우팅 ~5.5 µs, 2.8%; 계측 +148 ns. `dorang_request_duration_seconds`는 다른 것을 재고, 오버헤드를
> 분리하는 export 시리즈는 없다. 요청별 내역은 헤더(`X-Dorang-Queue-Ms`, `-Ttft-Ms`, `-Latency-Ms`)를 쓸 것.

### 7.3 shadow

| 알람 | 식 | 의미 | 조치 |
|---|---|---|---|
| 비용으로 shadow 중단 | `dorang_shadow_cost_capped == 1` | 일일 상한이 발동. 남은 UTC 하루 동안 shadow가 꺼졌고 리포트가 자라기를 멈췄다 | **리포트를 완결된 것으로 읽지 말 것.** `max_cost_usd_per_day`를 올리거나 `sample_rate`를 낮출 것 |
| 차이 발견 | `increase(dorang_shadow_with_diffs_total[1h]) > 0` | 참조 게이트웨이와의 구조적 발산 | JSONL 리포트를 읽을 것. [MIGRATION.ko.md](MIGRATION.ko.md) §4.5 |
| 결론 불가 | `increase(dorang_shadow_inconclusive_total[1h]) > 0` | 비교가 어떤 차원을 결정하지 못했다. **동일성의 증거가 아니다** | 레코드가 차원과 사유를 명시한다 |
| 작업 드롭 | `increase(dorang_shadow_dropped_total[1h]) > 0` | shadow 큐가 가득 차 작업이 버려졌다. 리포트로는 볼 수 없는 커버리지 구멍이 있다 | `queue_size`나 `workers`를 올리거나 `sample_rate`를 낮출 것 |
| 리포트 절단 | `increase(dorang_shadow_report_dropped_total[1h]) > 0` | 리포트가 바이트 상한에 도달. **잘린 리포트를 빈 리포트로 읽는 것이 이 메커니즘 최악의 결과다** | `report.max_bytes`를 올리고 파일을 회전한 뒤 다시 돌릴 것 |

### 7.4 이 빌드에서 알람을 걸 수 없는 것

⚠️ **계측 degradation 신호가 없다.** meter는 `trace_queue_full`, `spool_full`, `spool_error`,
`sink_error` 네 사유를 flapping하지 않도록 히스테리시스와 함께 추적하고, **아무것도 그것을 읽지 않는다**:
메트릭도, health 필드도, 로그 기반 카운터도 없다. 설계의 "드롭은 절대 조용하지 않다"는 이 빌드에서 참이
아니다. 연결되기 전까지는 spool 디렉터리 크기와 스토어 자체의 상태를 대신 볼 것이며, §11.4를 알람이 아니라
진단 절차로 다룰 것.

축별 capacity 점유율, 크리덴셜 상태, 프로바이더 쿼터 퍼센트, 예산 소비율, prefix 적중률, 사유별 폴백에
대한 export 메트릭도 없다. 그 상태는 capacity broker, health tracker, prefix 테이블, cluster 노드에
존재하고, 어느 것도 `/metrics`에 도달하지 않으며, 그것을 노출할 관리 API는 마운트돼 있지 않다(§0.2).

---

## 8. 백업과 복구

### 8.1 무엇을 백업해야 하는가

| 항목 | 위치 | 잃으면 |
|---|---|---|
| 스토어 | `storage.sqlite.path` 또는 PostgreSQL | 발급된 키, 예산과 지출, 원장, batch 상태, response store 행 |
| key pepper | `$DORANG_KEY_PEPPER`, **또는 SQLite DB 옆 파일** | 발급한 모든 키가 검증 불가능해진다. 복구 방법이 없다 — 토큰은 저장돼 있지 않다 |
| master key | `$DORANG_MASTER_KEY` | 관리 접근 |
| 설정 | `--config` 경로 | 재구성 가능하지만 느리다 |
| 가격 카탈로그 | `pricing.catalog` | 과거 수치는 원장에 남고, 새 요청은 UNPRICED가 된다 |
| 모델 카탈로그 오버레이 | `$DORANG_CATALOG_PATH` 레이어 | 컨텍스트 윈도우와 능력이 내장 기본값으로 떨어지며, 그것이 §4.3이 경고하는 "너무 큰" 방향이다 |
| 프로바이더 비밀값 | `key_env` / `key_file`이 가리키는 곳 | 전부 멈춘다 |

spool(`metering.spool.dir`)은 의도적으로 이 목록에 **없다.** 이동 중인 트레이스 페이로드를 담으며, 잃는
것은 발췌이지 회계가 아니다. 수치 회계는 절대 그곳을 거치지 않는다.

### 8.2 SQLite

```
systemctl stop dorang
sqlite3 /var/lib/dorang/dorang.db ".backup '/backup/dorang-$(date -u +%FT%TZ).db'"
cp /var/lib/dorang/dorang.db.pepper /backup/     # pepper가 생성된 경우
systemctl start dorang
```

`.backup`은 라이브 DB에서도 안전하지만, 먼저 멈추면 pepper 파일과 spool까지 일관된 시점에 고정된다.
**데이터베이스와 pepper를 함께 백업할 것** — 하나의 산출물이다.

### 8.3 PostgreSQL

```
pg_dump --format=custom --no-owner "$DORANG_DATABASE_URL" > dorang-$(date -u +%F).dump
```

원장이 큰 테이블이고 일별 파티션이다. 덤프 시간이 문제라면 `request_logs`와 `request_traces`를 제외하고,
복구가 히스토리를 잃되 키·예산·지출 상태·batch 상태는 유지한다는 것을 받아들일 것 — 게이트웨이를 서빙하게
만드는 것은 그쪽이다.

### 8.4 복구

```
systemctl stop dorang
# 데이터베이스 복구 후:
dorangctl migrate --config /etc/dorang/config.yaml
dorangctl config lint /etc/dorang/config.yaml
systemctl start dorang
dorangctl key list --config /etc/dorang/config.yaml | head
```

**같은 pepper로** 복구할 것. 키 목록에는 행이 보이는데 모든 요청이 401을 답한다면 pepper 불일치의 시그니처다.

⚠️ **내구 상태는 복구 시 채택이 아니라 검증되어야 한다.** 구조적 상한이 없는 영속
`credential_state.unavailable_until`은 저장된 장애다: 한 달 된 백업을 복구하면 크리덴셜이 아무 관련 없는
날짜까지 unavailable로 표시된 채 돌아올 수 있다. 오래된 백업을 복구한 뒤 `credential_state`를 확인하고, 다른
시대의 타임스탬프를 믿기보다 쿨다운을 리셋하는 쪽을 택할 것.

---

## 9. 업그레이드

스키마는 버저닝돼 있고 마이그레이션은 내장이며 전진 전용이다. 다운그레이드 경로가 없으므로 롤백 계획은
데이터베이스 복구다.

**단일 노드:**

```
dorangctl config lint /etc/dorang/config.yaml         # 새 dorangctl로
dorang --config /etc/dorang/config.yaml --check       # 새 dorang으로
cp dorang /usr/local/bin/dorang.new && mv /usr/local/bin/dorang.new /usr/local/bin/dorang
systemctl restart dorang
```

**fleet, 롤링:**

1. 백업(§8)하고 스키마 버전을 기록: `dorangctl migrate`가 보고한다.
2. 롤링 전에 한 노드에서 마이그레이션을 **한 번** 적용: `dorangctl migrate`.
3. 노드를 하나씩 롤. 각 노드가 멈추기 전에 드레인한다(§10).
4. 롤 내내 `dorang_ready`, `dorang_responses_total{class="5xx"}`, `dorang_auth_failures_total`을 볼 것.

순서 규칙 둘:

- **롤링 중이 아니라 롤링 전에 마이그레이션할 것.** 서로 다른 스키마 버전의 두 바이너리가 같은 원장에
  쓰는 것은 스키마 레이스이고, 파티션 writer의 인라인 복구는 없는 파티션을 위한 것이지 없는 컬럼을 위한
  것이 아니다.
- **설정 변경과 바이너리 변경은 같은 단계여서는 안 된다.** 새 바이너리가 옛 설정을 거부하면 둘 중 어느
  것 때문인지 알고 싶을 것이다.

새 바이너리로 옛 설정에 `--check`하는 것이 가장 값싼 사전 점검이고, 실제로 일어나는 경우를 잡는다: 새
스키마가 더 이상 받지 않는 키, 혹은 이제 요구하는 키.

---

## 10. 노드 드레인

`SIGTERM` 또는 `SIGINT`가 드레인을 시작한다. 순서:

1. **readiness가 즉시 `503`으로 뒤집힌다.** `dorang_ready`가 0이 된다. liveness는 `200`을 유지한다.
2. 리스너가 즉시 닫히므로 **새 연결은 TCP 수준에서 거부된다** — "리슨은 유지한 채 새 작업을 503으로
   거부"하는 모드는 없다. 새 요청이 거부되는 대신 다른 곳으로 라우팅되기를 원한다면 시그널 *전에* 노드를
   로드밸런서에서 빼야 한다.
3. 진행 중 요청은 **완료까지 실행되고 정상 상태를 받는다.** 이미 서빙 중인 요청에 503이 주입되지 않는다.
4. 그것들이 끝나거나 `server.shutdown_grace`가 만료되면 서버가 하드 종료한다.
5. 그 뒤 프로세스가 구성의 역순으로, 같은 grace로 제한되어 해체된다: shadow 워커 풀, meter(디스크로
   드레인하고 마지막 flush를 한 번 시도), 그다음 스토어. **쓰이지 않은 예산 리스 블록이 반환되므로** 계획된
   재시작은 정확하다 — 내구 카운터가 정확히 쓴 만큼을 담는다. 크래시는 이 단계를 건너뛰고, 그 차이가 공표된
   초과분 전부다.

grace가 만료됐는데 아직 진행 중 작업이 있으면 프로세스는 **1**로 종료하며 다음을 낸다:

```
dorang: drain grace expired with requests still in flight
```

노이즈가 아니라 진짜 신호다: 설정한 창 이후에도 무언가가 돌고 있었다.

**pre-stop 패턴.** readiness를 종료 자체보다 먼저 뒤집을 수 있어, 요청이 여전히 성공하는 동안 로드밸런서가
작업 전송을 멈춘다. Kubernetes에서는 readiness 프로브 탐지 창보다 긴 `preStop` sleep이 dorang 쪽 설정 없이
같은 효과를 낸다:

```yaml
lifecycle:
  preStop: {exec: {command: ["/bin/sleep", "10"]}}
terminationGracePeriodSeconds: 130      # 2 × shutdown_grace + 여유
```

크기 규칙: `terminationGracePeriodSeconds > 2 × server.shutdown_grace`. 드레인과 해체가 각각 grace 전부를
받기 때문이다.

**클러스터에서는** 리더의 일 — 롤업 압축, 파티션 유지보수, capacity·예산 예약 sweep, batch 할당, 리스
재분배 — 이 리더가 떠날 때 선출로 옮겨간다. 리스와 예약은 드레인의 일부로 반환된다. `cluster.node_id`가
안정적인 노드는 재시작 시 만료를 기다리는 대신 자기 리스를 회수하며, 그것이 CONFIG §4가 그 설정을 고집하는
이유다.

---

## 11. 문제 해결

여기 있는 것들은 설계가 이미 문서화한 실패 모드들이고, 그래서 실제로 일어날 것들이다. 각각이 진단하기
어려운 이유는 같다: **아무것도 에러를 반환하지 않는다.**

### 11.1 백엔드가 요청받은 것을 무시한 채 200을 반환한다

**형태.** 두 self-hosted 엔진 모두 요청을 받아들이고, 설정되지 않은 필드를 무시하고, `200`을 답한다.
응답 어디에도 그렇다는 말이 없다. vLLM과 SGLang 통합 리스크에서 가장 큰 부류이며, 두 엔진의 오퍼레이터
프로필이 *플래그가 하는 일*이 아니라 *없을 때 조용히 깨지는 것*을 열거하는 이유다.

| 관찰 | 유력한 원인 | 확인 | 조치 |
|---|---|---|---|
| `priority`가 스케줄링에 영향이 없다 | 어느 쪽이든 기본 스케줄러 정책 | vLLM: 응답으로 전혀 탐지 불가 — 정책을 노출하는 유일한 엔드포인트가 개발 모드다. SGLang: `--abort-on-priority-when-disabled`를 설정하면 버리는 요청 하나가 안정적 메시지의 503을 반환 | vLLM `--scheduling-policy priority`; SGLang `--enable-priority-scheduling` |
| SGLang에서 batch가 realtime을 앞지른다 | 두 엔진이 priority를 **반대 방향**으로 정렬하고 둘 다 200을 반환 | 방출된 와이어 값을 엔진 방향과 대조 | `--schedule-low-priority-values-first`, 또는 프로바이더별 emit 방향 설정 |
| 툴 호출이 평문으로 오고 `finish_reason: "stop"`, `tool_calls: null` | `--tool-call-parser` 없는 SGLang. 툴이 프롬프트에 렌더링되고 모델의 네이티브 문법이 `content`에 착지 | 컨텐트에 모델의 툴 문법이 그대로 있다 | `--tool-call-parser <p>` |
| vLLM에서 `tools`가 있는데 클라이언트가 컨텐트만 받고 에러 없음 | `tools`와 함께 `tool_choice: null`. 명시적 null이 검증을 통과한 뒤 파서를 절대 호출하지 않는 분기로 간다 | 요청 본문에 리터럴 `null`이 있는지 확인 | dorang이 리터럴 `null`을 `"auto"`로 정규화한다; 패스스루로 그것을 우회하고 있다면 보내지 말 것 |
| `reasoning` / `reasoning_content`가 항상 null | 두 엔진 모두 `--reasoning-parser` 미설정 | `<think>` 태그가 `content`에 인라인으로 남는다 | 파서를 설정할 것. 필드 이름이 다르다: vLLM `reasoning`, SGLang `reasoning_content` |
| 맞지 않았어야 할 요청이 정상처럼 보이는 답과 함께 200 | 백엔드가 조용히 절삭했다. 어떤 것은 과대 입력을 클램프하고, 최소한 하나는 절삭한 뒤 **출력 토큰 0**으로 `length` stop을 반환한다 | 보고된 `input_tokens`를 선언 윈도우와 비교. 윈도우를 넘는 보고 입력이 시그니처 | 백엔드 자체 auto-truncation을 절대 켜지 말 것. 출력 0의 `finish_reason: "length"`는 completion이 아니라 오버플로다 |
| 쿼터 에러가 HTTP 200으로 온다 | 어떤 벤더는 성공 상태 안에 에러 봉투를 반환 | 본문이 에러 객체가 아니라 벤더 상태 필드를 싣는다 | dorang은 HTTP 상태로 실패를 분류한다; `200`으로 도착한 쿼터 소진은 재시도되지도 장애로 기록되지도 않고 **성공 요청으로 과금된다** |

**일반 규칙.** `200`은 요청이 요청받은 대로 서빙됐다는 증거가 아니다. dorang이 이미 가진 숫자로 교차
검증할 수 있는 곳 — 보고된 입력 토큰 대 선언 윈도우, `cached_tokens` 대 플래그 설정 여부 — 에서는 그렇게
하고, 할 수 없는 곳에서는 프로바이더 설정의 오퍼레이터 선언이 유일한 진실이며 dorang은 가정하는 대신 미검증
능력으로 표시한다.

### 11.2 유휴해서가 아니라 플래그가 없어서 0으로 읽히는 메트릭

**형태.** 부하 신호가 설정되지 않았을 때 가장 매력적인 값을 읽고, least-busy 라우터가 그것을 믿는다.

| 신호 | 0의 의미 | 구별법 |
|---|---|---|
| vLLM `/metrics`가 **`vllm:` 시리즈 없이** 200 | `--disable-log-stats`가 설정됨 | 엔드포인트가 도달 가능하고 본문에 `vllm:` 줄이 없다. "도달 가능, 시리즈 없음"과 "유휴"는 똑같아 보이면서 정반대를 뜻한다 |
| vLLM `/load`가 **영원히** `{"server_load": 0}` | `--enable-server-load-tracking` 미설정 | 설정되지 않은 `0`은 진짜 유휴와 구별되지 않으면서 least-busy 라우터에게 가장 매력적인 값이다. **플래그를 독립적으로 확인하지 않고 `/load`를 쓰지 말 것** |
| SGLang `sglang:cache_hit_rate`가 대부분 0 | 플래그가 아니라 — 게이지가 매 decode 리포트마다 `0.0`으로 하드 리셋된다 | 대신 `sglang:cached_tokens_total`과 `sglang:prompt_tokens_total`에서 계산할 것 |
| SGLang `sglang:utilization`이 0 또는 **`-1`** | 입력에 setter가 없고, PD-prefill 모드에서 `-1` | 음수 utilization은 순진한 임계값 검사를 전부 통과한다 |
| SGLang `/v1/loads`가 전부 0 | writer 생성 시점에, 어떤 forward pass보다 먼저 0 스냅샷이 기록된다 | **`max_total_num_tokens == 0`은 "유휴"가 아니라 "준비 안 됨"으로 취급할 것.** HTTP 200의 빈 `{"loads": []}`는 공유 메모리 attach 실패, 즉 유휴가 아니라 고장 |
| SGLang `/metrics`가 **404** | `--enable-metrics` 미설정 | 정직한 실패이자 더 나은 실패다: 상태 코드만으로 구별 가능 |

**dorang 안에서도 같은 규칙이 라우팅 입력에 적용된다.** 세 신호가 부재할 수 있고, 각각 부재가 조용히
*이기는* 방향이 있다:

| 신호 | 부재의 의미 | 함정 |
|---|---|---|
| 지연 샘플 | 미검증 | 0이 **가장 빠른** 값 — 미검증 백엔드가 무지로 이긴다 |
| 처리량 샘플 | 미검증 | 0이 **가장 느린** 값 — 영원히 지고, 경쟁할 수 있게 해 줄 샘플을 영영 얻지 못한다 |
| 가격 | 가격 없음 | 0이 **가장 싼** 값 — 아무도 가격 매기지 않은 배포가 영구히 조용히 모든 그룹을 이긴다 |

셋 다 **의견 없음**으로 해소된다: 그 comparator는 후보를 건너뛰고, 체인의 다음 comparator가 순위를 매기며,
카운터가 기록한다. 라우팅이 이상한데 에러가 없다면, 어느 comparator가 고장인지가 아니라 어느 comparator에
데이터가 없었는지를 물을 것.

### 11.3 버스트 중의 낡은 쿼터 값

**형태.** 프로바이더 보고 쿼터 폴은 최대 `usage_probe.interval`만큼 낡았다. 버스트 중에는 로컬 카운터가 더
신선하고, 프로바이더 값이 더 완전하다. dorang은 둘을 결합한다:

```
effective_used = max( provider_reported_used ,
                      provider_reported_used_at_last_poll + local_delta_since_that_poll )
```

늦은 폴이 버스트를 지울 수 없다. 쿼터 결정을 진단한다면:

1. **실패한 조회는 절대 크리덴셜을 비활성화하지 않는다.** 실패한 읽기는 소진된 쿼터가 아니다. 마지막
   정상 스냅샷이 유지되고 낡음 정도가 노출된다.
2. 보고 퍼센트는 마지막 폴 + 로컬 델타에서 오므로 폴 사이에 뒤처져 보이는 것이 정상이다. 그 간극이
   중요하면 `usage_probe.interval`을 줄일 것.
3. 응답의 `x-dorang-quota-<window>-used-pct`(`X-Dorang-Detail: full`과 함께)가 그 요청에 대해 라우터가 본
   값이다.
4. ⚠️ **롤링 윈도우는 `capacity_mode: local`에서만 정확하다.** 롤링 5시간 허용량에는 자연스러운 period
   경계가 없어 공유·리스 모드가 epoch 정렬 그리드에 키잉한다. 그리드에서 리셋되는 "5시간"은 실제 근사다.
5. ⚠️ **어떤 벤더는 필드 이름이 *소비량*이라고 말하는 엔드포인트로 *잔량*을 보고한다.** 잔량을 사용량처럼
   결합하면 부호가 뒤집힌다. 퍼센트를 믿기 전에 fetcher를 벤더의 실제 의미론과 대조할 것.

### 11.4 재시작 후 예산이 소진된 것처럼 보인다

**형태.** 예산은 미룰 수 없는 유일한 쓰기다 — 예약을 미루면 동시 요청 둘이 모두 지출 전 잔액을 보게 되고,
그것이 reserve-before-spend가 막으려는 바로 그것이다. 그래서 동기이되 값싸게 만들어졌다: 핫패스는 원자적
연산으로 보호되는 노드 내 메모리 예약을 건드리고, 내구성은 노드가 이미 쥐고 있는 **리스 블록**에서 온다.
스토어는 요청당이 아니라 블록당 쓰기를 본다 — 요청 400건에 스토어 쓰기 5회로 측정됐다.

**graceful** 정지는 모든 블록의 쓰이지 않은 부분을 반환하므로 계획된 재시작이 정확하다. **크래시**는 그러지
않고, 반환되지 않은 나머지가 부과된다. 그것이 공표된 초과분이며 블록 크기로 제한된다: 기본 블록은 $0.05다.

재시작 후 예산이 예상보다 낮게 읽히면:

| 원인 | 시그니처 | 조치 |
|---|---|---|
| 비정상 정지 | 간극이 노드당 최대 한 블록, 즉 ≤ $0.05 × 노드 수 | 없음. 상한이 있고 안전한 방향으로 부과된다 — 크래시는 허용량을 **덜** 쓰게만 할 수 있고 초과하게 하지 못한다 |
| 경합이 소진으로 보고됨 | 98% 남은 상한에 대한 종단 `400` | 실제 결함이었고 수정됐다; 옛 빌드에서 보인다면 업그레이드할 것 |
| 생각한 것과 다른 키 | 예산은 키에 붙고, 키의 예산 주기가 중요하다 | `dorangctl key list`가 키별 지출과 예산을 보여준다 |
| 예산 거부를 rate limit으로 읽음 | 응답은 코드 `budget_exceeded`의 **종단 `400`**, 의도적으로 `429`가 아니다 | 올바른 동작. `429`는 재시도를 유도하는 *동시에* 그 조건을 폴백 트리거로 표시하며, 그러면 호출자가 요청한 적 없는 모델에 **다른 주체의** 예산을 쓴다 |

더 이상 원인이 아니지만 과거에는 원인이었던 둘:

- 예산 상태가 메모리 전용이던 시절에는 재시작이 주기를 조용히 처음부터 시작했다. 이제 내구 원장에 예약되고
  재시작이 카운터를 다시 읽는다.
- 예산 강제는 한때 명세되고 구현되고 테스트되고 **아무 데도 연결되지 않았다**: 요청 경로가 그 경로의 무엇도
  증가시키지 않는 컬럼에 대해 지출을 검사했으므로, 예산은 참조된 적이 없어서 초과될 수 없었다. 모든 단위
  테스트가 통과했고 요구사항은 충족되지 않았다. 어떤 빌드에서든 예산이 강제되지 않는다면, 그 형태 — 양쪽
  끝에서 만족된 인터페이스가 어느 쪽에도 연결되지 않음 — 를 가장 먼저 확인할 것.

### 11.5 누군가를 호출하기 전에 알아 둘 만한 것들

| 증상 | 설명 |
|---|---|
| 이전이나 복구 후 모든 키가 인증에 실패 | pepper 불일치(§2.3, §8.4). 키 행은 있고 다이제스트가 다른 pepper로 계산됐다 |
| 폐기한 키가 아직 동작 | 크리덴셜이 캐시된다. 엔트리 TTL 이내나 `SIGHUP`에 해소된다 |
| 리로드가 "아무것도 안 했다" | 아마 성공했고 재구성 가능한 것만 재구성했다. 스토어, authenticator, meter, capacity broker는 리로드로 교체되지 **않는다** — 재시작이 필요하다 |
| shadow 설정 변경이 무시됨 | 올바르다. `shadow:`는 핫 리로드를 거부하는 유일한 절이다. 재구성이 일일 비용 상한을 재무장시키기 때문 |
| 패스스루 프리픽스가 501 | 그 프로바이더에 `base_url`이 없어 라우트가 빌드 시점에 삭제됐다 |
| `observability.prometheus: false`를 설정해도 `/metrics`가 계속 서빙 | 그 키는 절대 읽히지 않는다(§12) |
| 모델이 엉뚱한 배포로 라우팅되는데 결정 헤더는 맞아 보인다 | 과거에 정확히 이 형태가 있었다: pinned와 쿼터 필터된 후보 리스트가 하나의 공유 버퍼에서 잘려 나와, 모든 후보가 마지막 것의 프로바이더를 실었고 예약이 다른 배포의 축에 착지하면서 **결정은 올바른 정체성을 보고했다.** 결정에 대한 어떤 단언도 그것을 관측할 수 없다. 보이면 `X-Dorang-Deployment`와 capacity 스냅샷을 함께 확보할 것 |
| 두 축이 계속 바쁜 채로 다축 요청이 영원히 대기 | 리스크 W8, 열려 있음. 포화된 두 축이 필요한 대기자가 두 큐의 head에 앉은 채 둘이 동시에 비는 순간을 만나지 못할 수 있다. aging이 닫을 수 없다 — 대기자는 추월당하지 않고 그저 이기지 못한다. 두 축 중 하나를 넓힐 것 |

---

## 12. 설계돼 있으나 이 빌드에 없는 것

없는 것을 전제로 계획하지 않도록, 운용상 중요한 간극.

| 영역 | 상태 |
|---|---|
| **HTTP 관리** | 마운트됨(§3.1–3.3). 키, `/spend/logs`, capacity, catalog, health history, `/ui`는 서빙된다. 사용자·팀·배포·예산과 집계 지출 리포트는 `internal/store`에 해당 테이블 코드가 없어 `501 dependency_not_configured`로 답한다. 그것들은 `dorangctl`을 쓸 것 |
| **감사 기록 조회** | `audit_logs`는 모든 관리 변경이 기록하지만 `/audit/list`는 이름이 붙은 501이다. 테이블을 직접 조회할 것 |
| `observability.otlp_endpoint` | exporter가 연결돼 있지 않다. 지연 내역은 기록되고 export되지 않는다 |
| `capacity.*.rpm`, `.tpm` | **로드 시 거부**되며, 동작하는 자리를 이름으로 알려준다: 배포별 rate는 `deployments[].limits[]`, 호출자별 rate는 api 키 자신의 `rpm_limit`/`tpm_limit` |
| 노드 간 rate 제한 | 롤링 분은 프로세스별이다. N-노드 배포는 모든 `rpm_limit`, `tpm_limit`를 N배로 허용한다. durable 원장이 닫을 수 있으나 요청마다 store 쓰기가 든다 — 택하지 않았다 |
| `tpm_limit`는 **다음** 요청을 제한한다 | 토큰 수는 정산 시점에야 존재하므로, 거대한 요청 하나는 아무것도 거부하기 전에 상한을 한 번 넘을 수 있다 |
| **Lua** | **Lua 인터프리터가 없고**, 그것은 누락이 아니라 결정이다 — [CONFIG.ko.md](CONFIG.ko.md) §16. 네 훅 지점, 그 상한, 비밀값 없는 view는 구축돼 있다; 그 안에서 도는 것은 total 정책 언어(`*.policy`)이거나 컴파일된 Go `Native`다. `extensions.lua.dir` 아래의 `.lua` 파일은 조용히 무시되는 게 아니라 **로드 에러**다 |
| `on_route`에서의 재라우팅 | 훅은 선택된 배포를 보고 거부할 수 있지만 다른 것을 요구할 수는 없다 |
| 최상위 `quotas:`, `budget:` 블록 | 스키마에 없다. 예산은 `dorangctl key create --budget-usd`로 키별 |
| 기존 데이터베이스로부터의 크리덴셜 임포트 | 스토어에 구현돼 있고 **CLI 진입점이 없다** — [MIGRATION.ko.md](MIGRATION.ko.md) §3 |
| prefix / cluster 메트릭 | 상태는 존재하고 아무것도 export하지 않는다. capacity와 health는 `/admin/capacity`, `/health/history`에서 읽을 수 있다 |
| `providers[].usage_probe`, `providers[].metrics.interval`, `providers[].params.drop*`, `routing.prefix.checkpoints`, `deployments[].stream_timeout`, `key_rotation.…affinity_group`, `cluster.redis_url_env`, `observability.log_level`/`.log_format` | 로드되고 아무것도 하지 않는다. 목록은 `internal/config/consumed_test.go`에 실행 가능한 상태로 있어 드리프트할 수 없다 — [CONFIG.ko.md](CONFIG.ko.md) §23.1 |

---

## 함께 보기

- [CONFIG.ko.md](CONFIG.ko.md) — 모든 설정 키, 기본값, 틀렸을 때 깨지는 것.
- [MIGRATION.ko.md](MIGRATION.ko.md) — 기존 게이트웨이에서 옮겨오기.
- [COMPATIBILITY.ko.md](COMPATIBILITY.ko.md) — 클라이언트가 관찰하는 와이어 계약. 에러 분류표(§11)는
  아직 기준 문서 [COMPATIBILITY.md](COMPATIBILITY.md)에만 있다.
- [VLLM.ko.md](VLLM.ko.md) §5, [SGLANG.ko.md](SGLANG.ko.md) §8 — self-hosted 백엔드에 필요한 플래그.
- [DESIGN.ko.md](DESIGN.ko.md) §18 — 열린 리스크. 오퍼레이터가 실제로 마주칠 수 있는 것은 W8이다.
