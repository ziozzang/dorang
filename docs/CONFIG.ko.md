# 설정 레퍼런스

> 설정 스키마가 받는 모든 키: 타입, 기본값, 하는 일, 그리고 **틀렸을 때 무엇이 깨지는가**.
> 절 순서는 [DESIGN.ko.md](DESIGN.ko.md) §4.2를 따른다.
>
> 이 문서는 설계가 서술하는 설정이 아니라 **이 빌드가 읽는 설정**을 서술한다. 설계가 명세하지만
> 스키마가 받지 않는 키, 스키마가 받지만 아무것도 작동시키지 않는 키는 §23에 이름으로 열거한다.
> §24 전에 §23을 읽으면 반나절을 아낀다.
>
> 복사해 쓸 동작 파일: [`deploy/config.example.yaml`](../deploy/config.example.yaml).
> 테스트가 그것을 그대로 로드하므로 검증기와 어긋날 수 없다.
>
> **2026-07-28 작업 트리에 대해 검증됨.** 검증기를 읽는 대신 각 설정을 `dorangctl config lint`로
> 통과시켜 확인했다. 구현은 움직이고 있다: **부재 주장이 가장 먼저 만료되는 주장**이므로, §23의 무엇을
> 전제로 계획하기 전에 실제로 돌리는 빌드에 다시 대조할 것.
>
> English (기준 문서): [CONFIG.md](CONFIG.md)

---

## 0. 파일이 읽히는 방식

### 0.1 파일 하나, 엄격한 디코드

`version: 1`이 이 빌드가 이해하는 유일한 스키마 버전이다. 그 외에는 발견한 버전과 원한 버전을 명시하며
거부한다.

디코드는 세 가지 면에서 엄격하고, 각각 대안이 조용히 실패하기 때문에 존재한다:

| 규칙 | 대안의 결과 |
|---|---|
| **알 수 없는 키는 에러**, 무시할 줄이 아니다 | 오타는 그것이 바꾸려던 동작이 일어나지 않을 때까지 보이지 않는다 |
| 파일당 **정확히 하나의 YAML 문서** | `---` 뒤 두 번째 문서가 조용히 버려진다 |
| **모든 문제를 한 번에**, 각각 YAML 경로와 함께 | 결함 아홉 개짜리 파일은 왕복 아홉 번이 아니라 한 번에 고쳐야 한다 |

아홉이라는 숫자는 가정이 아니다: 검증기를 설계 자신의 예시 설정에 처음 돌렸을 때 이름만 있고 선언되지
않은 프로바이더·크리덴셜·capacity 그룹·모델 참조 아홉 개를 찾아냈다.

**없는** 파일이 항상 에러인 것은 아니다. `--config`도 `$DORANG_CONFIG`도 없으면 서버는 작업 디렉터리의
`dorang.yaml`을 찾고, 없으면 문서화된 기본값으로 기동한다: SQLite, 업스트림 없음, `:4100` 리슨.
명시적으로 지정한 파일이 없으면 그것은 **에러**다. 선택이 아니라 오타이기 때문이다.

### 0.2 스칼라 형식

| 형식 | 허용 | 비고 |
|---|---|---|
| Duration | `250ms`, `30s`, `1h`, `2h45m` | **맨 숫자는 초** — `timeout: 180`은 180s. 음수 거부 |
| Size | `4096`, `64MiB`, `8GiB`, `512KB` | `KiB/MiB/GiB/TiB/PiB`는 1024의 거듭제곱; `KB/MB/GB/TB/PB`는 1000의 거듭제곱. 대소문자 무관 |
| Decimal | `"2.50"`, `"20.00"`, `"5"` | **텍스트로 보관**되어 정확히 파싱된다. 가격은 절대 이진 부동소수를 거치지 않는다(§8.3). 따옴표로 감쌀 것 — 아니면 YAML이 float를 준다. **지수 표기(`1.25e-7`)는 거부되고**, 소수점 아래 12자리를 넘는 값도 거부된다: 자릿수로 풀어 쓸 것(§13.1c) |
| Path | `~/.dorang/dorang.db` | 선행 `~`는 프로세스 사용자 홈으로 확장된다 |

### 0.3 비밀값

비밀값은 이 파일에 절대 나타나지 않는다. 모든 키 자료 필드는 참조이며, 아래 중 **정확히 하나**만
설정한다:

```yaml
key_env:  NAME             # 프로세스 환경에서 읽는다
key_file: /path/to/key     # 파일에서 읽고 후행 개행을 제거한다
key:      literal          # server.env가 "development"일 때만 허용
```

하나도 설정하지 않으면 에러. 둘 이상이면 에러. 설정되지 않았거나 빈 변수를 가리키는 `key_env`도 에러이고,
읽을 수 없거나 빈 `key_file`도 에러다.

`key_ref`는 **거부된다.** 예전에는 로드되고 검증되면서 크리덴셜에 쓸 수 있는 비밀값을 남기지 않았다:
그 요청은 `Authorization` 헤더 없이 업스트림으로 나갔고, 실패는 프로바이더의 `401`로 도착했으며 파일
안에는 그것을 설명할 것이 아무것도 없었다. 이 빌드에는 resolver가 없으므로 그 키는 대신
`key_env`와 `key_file`을 지목하는 로드 에러다 — 파일을 쓰거나 변수를 내보내는 볼트 에이전트면 둘 다
만족한다.

해결된 비밀값은 모든 렌더링 경로에서 도달 불가능하다 — `String`, `%v`, `%#v`, YAML/JSON 마샬, 그리고 이
패키지가 만드는 모든 에러가 값이 아니라 가려진 *출처*(`key_env:PLAN_A_KEY`)를 반환한다. 인라인 리터럴은
해결 과정에서 필드 밖으로 옮겨져 파일이나 로그 줄로 다시 마샬될 수 없다.

### 0.4 기본값

이 문서의 모든 기본값은 로드 시 적용되고, 멱등이며, 파일이 설정한 값을 절대 덮지 않는다. 명시적 `false`나
`0`이 "미설정"과 다른 필드는 바로 그 이유로 포인터로 실린다: `metering.numeric.enabled: false`는 부재가
아니라 거부이고, 검증기가 그렇게 취급한다.

가장 짧은 동작 파일은 `version: 1` + `providers` + `credentials` + `models`다.

### 0.5 핫 리로드 — 그리고 하지 않는 두 절 

설정은 파일 변경, `SIGHUP`, 관리 리로드 엔드포인트로 다시 읽힌다. 파일은 2초마다 stat되고, 대부분의
편집기와 형상 관리 도구가 쓰는 rename-into-place는 타임스탬프뿐 아니라 identity도 바꾸므로 탐지된다.

의지할 만한 성질 셋:

- **실패한 리로드는 아무것도 바꾸지 않는다.** 다시 읽고, 기본값을 채우고, 해결하고, 검증한 뒤에야
  교체한다. 깨진 편집은 부분 적용이 아니라 "변화 없음"과 로그된 에러로 degrade한다. 이것은 에러의 부재가
  아니라 포인터 동일성으로 단언된다.
- **진행 중 요청은 시작할 때의 스냅샷을 유지한다.** 리더는 원자적 포인터를 취하고 락을 잡지 않는다.
- **전부가 재구성되지는 않는다.** 라우터, 가격 카탈로그, 업스트림 테이블은 재구성되어 교체된다. 스토어,
  authenticator, meter, **capacity broker는 아니다** — broker 교체는 살아 있는 예약을 떨구고, 스토어 교체는
  진행 중 원장 쓰기 아래의 커넥션 풀을 떨군다. 따라서 `storage.*`나 capacity 상한 변경에는 `SIGHUP`이
  아니라 재시작이 필요하다.

⚠️ **`shadow:`는 의도적으로 핫 리로드하지 않는다.** 일일 비용 상한, 샘플 집합, 리포트 핸들이 프로세스별
상태다. 재구성은 상한을 재무장시켜 "하루 $5"를 "`SIGHUP`당 $5"로 바꾼다. 바뀐 `shadow` 절은 **거부**되고
돌던 설정이 유지된다.

⚠️ **`auth: oauth` 크리덴셜 집합도 같은 부류의 이유로 핫 리로드하지 않는다 (§7.2).** 각 크리덴셜은 메모리
안의 액세스 토큰, 백오프를 재는 연속 실패 횟수, 그리고 시작 시 한 번 띄운 갱신 루프를 갖는다. `SIGHUP`마다
재구성하면 모든 저장소를 다시 읽고, 이미 실패 중인 계정의 백오프를 재무장시킨다. 집합을 추가·삭제·이동하는
리로드는 재시작하라는 메시지와 함께 **거부**되고, 집합을 건드리지 않는 리로드는 평소대로 적용된다.

### 0.6 서빙 전에 파일 검사하기

```
dorang --config /etc/dorang/config.yaml --check
dorangctl config lint /etc/dorang/config.yaml
dorangctl config lint --catalog /etc/dorang/models.d /etc/dorang/config.yaml
```

둘 다 서버가 돌리는 같은 검증기를 돌린다. `--check`에서는 **이 기계에서 읽을 수 없는** 비밀값이 경고로
낮아진다 — 배포의 키 자료가 없는 곳에서 설정을 린트하는 일이 일상이기 때문이다 — 반면 인라인 리터럴을
포함한 그 밖의 모든 것은 에러로 남는다. `dorangctl config lint`는 `$DORANG_CATALOG_PATH`가 지정한 것까지
포함해 모델 카탈로그 레이어도 린트하므로, 타이핑된 것이 아니라 서버가 로드할 것을 린트한다.

`--check`는 완전한 사전 점검이 아니다. §23.3이 그것을 통과하는 에러들을 열거한다.

---

## 1. 기동을 거부하는 세 가지 설정

설계가 셋을 명명한다. 각각 경고가 아니라 하드 거부이며, 규칙보다 그 근거가 더 값지다 — 이유를 이해한
오퍼레이터는 우회하는 데 하루를 쓰지 않는다.

### 1.1 클러스터링 + 로컬 capacity 회계

```yaml
cluster:
  enabled: true
  capacity_mode: local      # 기동 거부
```

**규칙.** `cluster.enabled: true` + `capacity_mode: local`은 기동을 거부한다. `shared-pg`나 `leased`를
쓸 것. (`shared-redis`는 네 번째 모드이고, 다른 이유로 이 빌드에서 거부된다: 프로토콜은 실려 있고 그것을
말하는 클라이언트가 없다.)

**왜 권고가 아닌가.** 노드 로컬 카운팅은 한 노드에서 *정확*하다. N 노드에서는 모든 상한이 N번 세어지므로
7짜리 프로바이더 상한이 최대 7N 동시 요청을 허용한다. 여기까지는 자명하다. 자명하지 않은 것은 피해가
어디에 착지하느냐다.

프로바이더는 초과분에 `429`로 답한다. `429`는 `rate_limit`으로 분류되고 그 기본 폴백 체인은
`[same_group, same_class]`(§12)이다. 그래서 초과는 초과한 모델에 에러를 내는 데 그치지 않고 **같은 클래스의
다른 모든 모델의 capacity를 쓴다** — 거기로 라우팅된 적 없는 요청들에 대해. 포화된 코딩 플랜이 아무도
건드리지 않은 chat 모델을 세 홉 떨어진 곳에서 degrade시키고, 움직인 메트릭은 엉뚱한 배포에 있다. 실패가
원인에서 멀리 나타나는 이 성질이 이것을 단지 틀린 게 아니라 진단 비용이 비싼 것으로 만든다.

**모든 모드가 최대 초과분을 숫자로 공표한다.** "대략 정확"은 받아들일 수 있는 명세가 아니다:

| 모드 | 최대 초과 | 핫패스 비용 |
|---|---|---|
| `local` | `limit × (nodes − 1)` | 없음 |
| `shared-redis` | **0** | +1 RTT |
| `shared-pg` | **0** | +1 RTT, Redis보다 큼 |
| `leased` | `block × (nodes − 1)` | 정상 경로에서 없음 |

공유 모드는 상한 "이하"가 아니라 *정확히* 상한만큼 admit함이 단언된다: 미달 admit하는 모드는 정확이 아니라
안전일 뿐이고, 오퍼레이터가 지불한 capacity를 못 쓰게 만든다.

### 1.2 development 밖의 인라인 리터럴 비밀값

```yaml
server:
  env: production
credentials:
  - {id: acct-1, provider: cloud-a, key: sk-literal}   # 기동 거부
```

**규칙.** `key:`는 `server.env: development`일 때만 허용된다.

**왜.** 설정 파일은 커밋되고, 티켓에 복사되고, 채팅창에 붙여지고, 템플릿 엔진이 렌더링하는 유일한
산출물이다. 그 각각이 `key_env` 참조에는 없는 노출 경로다. 규칙의 요지는 리터럴 자체가 불안전하다는 게
아니라, 비밀값을 담을 *수도 있는* 파일은 그것을 담은 것처럼 영원히, 모두가 다뤄야 한다는 것이다 — 그리고
그 규율은 새벽 3시 인시던트와 만나면 살아남지 못한다.

`development`는 규칙 때문에 랩톱을 못 쓰게 만들지 않기 위해 존재한다. 보안 자세가 아니다: 그 밖에는
아무것도 완화하지 않으므로, 프로덕션 배포의 키 처리를 우회하려고 `env`를 바꾸면 정확히 하나를 얻고 감사
기록을 잃는다.

### 1.3 종료일 없는 legacy 키 해싱

```yaml
auth:
  legacy:
    enabled: true
    until: ""            # 기동 거부
```

**규칙.** `auth.legacy.enabled: true`는 `auth.legacy.until: <date>`를 요구하고, 그 날짜는 미래여야 한다.
`2026-12-31`, `2026-12-31T00:00:00`, 완전한 RFC 3339 타임스탬프를 모두 받는다. 이미 닫힌 창은 닫힌 시각을
명시하며 기동을 거부한다.

**왜.** `legacy_sha256`은 unsalted 단일 라운드 다이제스트 — 흔한 기존 게이트웨이가 저장하는 방식이다.
받아들이면 기존 배포의 키가 마이그레이션 동안 계속 동작하고, 그것은 회피된 조정 비용만큼 실제 가치가 있다.
*영구히* 받아들이면 dorang의 키 데이터베이스가 영원히 오프라인 공격 가능해지고, 그 대가로 오후 한나절이면
끝날 마이그레이션을 영영 끝내지 않는다.

결정을 지은 숫자: 실제 배포에 대한 검증 결과 저장된 크리덴셜의 **대다수가 이미 만료**돼 있었다. 살아 있는
키 몇 개를 재발급하지 않으려고 약한 다이제스트를 영구 채택하는 것은 나쁜 거래이며, 그것이 좋아 보였던
이유는 원래 추정이 행이 살아 있는지 확인하지 않고 행 수만 셌기 때문이다.

그래서 legacy 검증은 **창**이고, 창에는 끝이 있다. `auth.rehash_on_use`(기본 켜짐)가 legacy 해시를 처음
검증에 성공할 때 `dorang_v1`으로 업그레이드하므로, 창은 flag day 없이 무중단으로 스스로 닫힌다.

### 1.4 로드가 아니라 나중에 실패하는 설정

검증기 의미의 기동 거부는 아니지만, 조립 시점이나 요청 시점에 실패하므로 `--check`가 통과하고 서버가 뜨지
않는다(또는 떠서 아무것도 하지 않는다). 운용상 거부처럼 동작하므로 여기 열거한다.

| 설정 | 언제 실패 | 무엇이 보이나 |
|---|---|---|
| `pricing.rules[].rates.images` | **로드.** `internal/pricing`에 이미지 단위가 없다 | `the images component is not priced by this build` — 대신 요청에 값을 매길 것. 예전에는 **조립**에서 실패했고 그래서 린트를 통과한 뒤 서버가 뜨지 않았다 (§23.1a) |
| `shadow.mode`를 켜고 `sample_rate` 미설정 | 절대. 돌면서 **아무것도** shadow하지 않는다 | 게이트 판정 `no_data`, 빈 리포트, 그리고 빈 리포트가 증거로 읽힌다 (§19) |
| `storage.driver: postgres` + URL 변수 미설정 | **조립.** 린트는 변수 *이름*만 확인 | 기동 시 스토어 open 실패 |

---

## 2. `server`

```yaml
server:
  listen: ":4100"
  env: production
  master_key_env: DORANG_MASTER_KEY
  key_pepper_env: DORANG_KEY_PEPPER
  request_timeout: 600s
  shutdown_grace: 30s
  pre_stop_delay: 10s
  max_body_bytes: 32MiB
  read_header_timeout: 30s
  read_timeout: 2m
  idle_timeout: 2m
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `listen` | string | `:4100` | 리슨 주소 `host:port` | 빈 값 거부. 커맨드라인 `--listen`이 우선하며, 한 파일로 두 인스턴스를 띄우는 방법이다 |
| `env` | `production` \| `development` | `production` | 인라인 리터럴 비밀값 허용 여부(§1.2) | 프로덕션에서의 `development`는 커밋된 키를 유효한 설정으로 만든다 |
| `master_key_env` | string | `DORANG_MASTER_KEY` | 관리 크리덴셜을 담은 변수 이름. out-of-band로 상수 시간 비교되며 **행으로 저장되지 않는다** | 빈 값 거부. 변수 자체가 미설정이면 명시적 opt-out이 없는 한 authenticator가 구성을 거부한다 — 관리 인증을 잃는 것은 타이핑해서 명시해야 한다. 그러지 않으면 임포트된 DB만 읽는 게이트웨이가 조용히 관리자를 잃는다 |
| `key_pepper_env` | string | `DORANG_KEY_PEPPER` | `dorang_v1` 키 해싱용 HMAC pepper 변수 이름 | 빈 값 거부. **변수가 미설정이면 pepper가 한 번 생성되어 SQLite DB 옆에 기록된다.** 의존성 0의 노트북 티어를 유지시키지만, pepper가 DB 파일과 함께 다닌다 — 변수를 설정하지 않은 다중 노드 배포에서는 각 노드가 서로 검증할 수 없는 키를 발급한다 |
| `request_timeout` | duration | `600s` | 전체 요청 deadline. capacity 예약 만료(`request_timeout + 30s`)도 설정하므로 누수된 goroutine이 슬롯을 영구 점유할 수 없다 | 0 이하 거부. 너무 짧으면 긴 생성이 잘리고, 너무 길면 멈춘 업스트림이 그 창 내내 예약을 붙든다 |
| `shutdown_grace` | duration | `30s` | drain이 진행 중 요청을 기다리는 시간, 그리고 별도로 drain 이후 해체가 걸릴 수 있는 시간 | 음수 거부. p99보다 짧으면 롤링 재시작이 가시적 에러가 된다: 프로세스가 하드 종료하고 종료 코드 1과 `drain grace expired with requests still in flight`를 낸다 |
| `pre_stop_delay` | duration | `10s` | readiness가 false가 된 **후**, 리스너를 닫기 **전**까지 계속 서비스하는 시간. 폴링으로 unready를 감지하는 로드밸런서가 라우팅을 멈출 시간을 준다(§13). `프로브 주기 × 실패 임계치 + 프로브 타임아웃 + 엔드포인트 철회 전파` 로 잡을 것. `deploy/kubernetes.yaml`의 프로브는 5s가 필요하고 기본값은 5s의 여유를 남긴다 | 음수 거부. 너무 짧으면 롤링 재시작마다 밸런서가 알아채기까지 클라이언트가 connection-refused를 받는다 — graceful drain이 막으려던 바로 그 실패다. `0`은 명시적이고 정당하다: 단일 노드나 워크스테이션에는 라우팅하는 주체가 없다. 키가 없으면 0이 아니라 기본값이 적용되므로 `0`이라고 쓰면 정말 없음을 뜻한다. 두 번째 SIGTERM도 이 대기를 건너뛴다 |
| `max_body_bytes` | size | `32MiB` | 요청 본문 하나의 상한. 초과하면 **업로드 도중** `413 request_too_large`로 거부하며, 거부 메시지가 이 키의 이름을 말한다 | 거부 메시지는 늘 `max_body_bytes`를 언급했지만 **그런 설정은 존재하지 않았다**: 운영자는 없는 손잡이의 이름을 듣고 재빌드 외에는 방법이 없었다. 상한 없는 기존 게이트웨이로 40MiB 트랜스크립트나 base64 이미지 배치를 보내던 클라이언트는 전환 시점에 멈춘다. 상한 자체는 실재한다: 본문은 fallback을 가로질러 재생 가능하도록 버퍼링되므로(§15.5) 요청 하나가 붙들 수 있는 메모리를 정하는 값이다 — 파싱되는 가장 큰 숫자가 아니라 프로세스 메모리 예산에 맞춰 정할 것. 음수는 로드 시 거부되고, `0`은 미설정과 구분되지 않아 기본값이 적용된다 |
| `read_header_timeout` | duration \| `none` | `30s` | 요청 **헤더** 읽기의 상한 | 연결해 놓고 아무 말도 하지 않는 클라이언트는 핸들러까지 오지 않으므로 capacity 슬롯을 쓰지 않는다. 그것이 소진시키는 것은 리스너의 accept 큐이고, 그래서 셋 중 가장 짧다. `none`은 상한을 없앤다 |
| `read_timeout` | duration \| `none` | `2m` | 연결의 첫 바이트부터 헤더와 본문을 포함한 **요청 전체** 읽기의 상한 | 이것은 생성 예산이 아니라 **업로드 예산**이다: 응답을 제한하지 않으므로 몇 분씩 도는 스트림은 영향받지 않는다. 기본값은 최대 크기의 `max_body_bytes`를 약 2.9 Mbit/s로 실어 나른다 — 느린 회선으로 큰 오디오나 배치 입력을 올린다면 늘릴 것. **`read_header_timeout`보다 짧아서는 안 된다**: 둘 다 같은 시점부터 재므로 작은 쪽만 발화하게 되고, 그러면 작은 쪽이 큰 쪽을 자기 이름으로 강제한다. 이것은 `dorangctl config lint`에서, 그리고 서버가 구성될 때 다시 **로드 에러**다 |
| `idle_timeout` | duration \| `none` | `2m` | keep-alive 연결의 요청과 요청 **사이** 상한 | Go의 기본값은 이 값에 `read_timeout`을 쓰는데, 그러면 둘이 조용히 하나의 설정이 된다. 둘은 다른 질문에 답하므로 갈라 두었다. 앞단에 있는 것의 idle timeout보다 **길게** 잡을 것(AWS ALB 60s, nginx `keepalive_timeout` 75s): 유휴 연결을 닫는 쪽은 그것이 유휴임을 아는 쪽이어야 한다. 게이트웨이가 먼저 닫으면 프록시는 죽은 소켓을 재사용하다가 알게 되고, 클라이언트는 나가지도 않은 요청에 대해 502를 받는다 |

### 2.1 세 가지 연결 deadline, 그리고 `none`의 의미

셋 다 `0`/부재를 "기본값을 쓰라"로, **`none`**이라는 단어를 "여기에는 상한을 두지 말라"로 받는다 —
`pre_stop_delay`가 필요로 하는 것과 같은 두 값의 구분이고, 이유도 같다. "끔" 값이 "미설정" 값과 겹치는
설정은 꺼 둔 기능이 다음 리팩터링에서 다시 켜지는 방식이다. 리터럴 음수 duration(`-1s`)은 `none`의
동의어로 받아들여져 그것으로 정규화된다. `-1s`와 `-5s`가 서로 다른 양의 "제한 없음"을 뜻할 수는 없기
때문이다.

`none`은 도피구가 아니라 진짜 답이다: 같은 deadline을 이미 강제하는 프록시 뒤의 배포는 그것을 두 번
강제하고 있으며, 둘 중 짧은 쪽이 아무도 설정하지 않은 방식으로 이긴다. 그리고 이 deadline들이 대체한
무제한 동작으로 돌아가는 유일한 경로이기도 하다.

> **셋 다 동작하는 소비자를 가진 `server.Options` 필드로 존재했고, 어떤 설정도 거기에 닿을 수 없었다.**
> 배선되기 전의 `compat.legacy_headers`와 같은 형태다: 코드는 옳았고, 테스트는 통과했고, 모든 배포가
> 파일에 무엇을 쓰든 내장된 숫자로 돌았다 — 파일이 아무 말도 할 수 없었기 때문이다. `internal/config`의
> 재발 방지 가드(`TestEveryConfiguredFieldIsReadSomewhere`)는 이들에 대해 **공허**하다: 식별자 이름으로
> 매칭하는데 `ReadTimeout`과 `IdleTimeout`은 `internal/server` 자신의 소스에 나오므로 내내 소비된 것으로
> 보고했다. 그 가드의 doc 주석이 스스로 열거하는 사각지대다. 지금 이들을 붙들고 있는 것은
> `config.LoadBytes`를 조립된 게이트웨이에 통과시키고 돌고 있는 서버가 소켓을 닫는지 닫지 않는지를
> 단언하는 `internal/app`의 설정별 행동 테스트다.

---

## 3. `storage`

```yaml
storage:
  driver: sqlite
  sqlite:   {path: ~/.dorang/dorang.db}
  postgres: {url_env: DORANG_DATABASE_URL, max_conns: 32}
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `driver` | `sqlite` \| `postgres` | `sqlite` | 원장과 컨트롤 플레인 스토어 선택 | 그 외 거부. SQLite 드라이버가 순수 Go라 바이너리가 정적이고 노트북 티어에 설치할 것이 없다 |
| `sqlite.path` | path | `~/.dorang/dorang.db` | DB 파일. 부모 디렉터리를 `0700`으로 생성 | driver가 `sqlite`일 때 빈 값 거부. 이 파일은 생성된 key pepper(§2)도 싣고, 원장·예산·발급된 키가 여기 산다 — 잃으면 넷 다 잃는다 |
| `postgres.url_env` | string | `DORANG_DATABASE_URL` | 접속 URL을 담은 변수 이름 | driver가 `postgres`일 때 빈 값 거부. **변수 내용은 로드 시 검사되지 않고** 이름만 확인하므로, 잘못된 URL은 린트가 아니라 기동에서 실패한다 |
| `postgres.max_conns` | int | `32` | 커넥션 풀 상한 | 0 이하 거부. 너무 작으면 원장 writer가 요청 경로의 예산 예약과 직렬화되고, 너무 크면 fleet 전체가 서버의 `max_connections`를 소진한다 |

마이그레이션은 내장되어 기동 시 적용된다. SQLite와 PostgreSQL이 하나의 스키마 정의를 공유하며, 방언별인
것은 넷뿐이다 — placeholder 문법, 마이그레이션 락, 파티셔닝, missing-partition 탐지. 모든 컬럼의
타임스탬프는 정수 마이크로초다. SQLite에 timestamp 타입이 없어 한쪽이 양보해야 했고, 정수는 정확하고
정렬이 맞고 타임존을 싣지 않으며 일별 파티션 경계를 산술로 만든다.

PostgreSQL에 **기본 파티션은 의도적으로 없다.** 기본 파티션은 missing-partition 실패를 시끄러운 것에서
조용한 것으로 바꾸고, 나중에 진짜 파티션을 붙이려면 기본 파티션에 쌓인 전부를 스캔해야 한다. writer는
대신 미리 파티션을 만들고, 서버가 행에 대한 파티션이 없다고 보고하면 인라인으로 복구한다.

---

## 4. `cluster`

```yaml
cluster:
  enabled: false
  node_id: ""
  redis_url_env: DORANG_REDIS_URL
  capacity_mode: local
  min_leasable: 16
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `enabled` | bool | `false` | 다중 노드 동작: 리더 선출, 리스 조정, §1.1 가드 | fleet에서 `false`로 두면 각자 자기 상한을 세는 독립 게이트웨이 N개가 된다 — §1.1이 거부하는 바로 그 초과를 설정 대신 누락으로 얻는다 |
| `node_id` | string | `""` | 리스 행에서 이 프로세스의 이름 | 빈 값은 **프로세스마다** 하나를 파생하며, 파일을 어떻게 복사하든 충돌할 수 없는 유일한 값이다. 대가는 재시작한 노드가 자기 리스를 회수하지 못하고 만료를 기다린다는 것. 설정한다면 **모든 노드에서 서로 달라야 한다** — 아래 참조 |
| `redis_url_env` | string | `DORANG_REDIS_URL` | Redis URL 변수 이름 | 아무것도 다이얼하지 않는다. 이것을 읽을 유일한 모드가 로드에서 거부되며(아래), LiteLLM에서 가져온 설정이 그대로 파싱되도록 키만 남겨 두었다 |
| `capacity_mode` | `local` \| `shared-redis` \| `shared-pg` \| `leased` | `local` | capacity **및 쿼터**의 노드 간 조정 방식. 정확도 어휘는 둘이 아니라 하나 | §1.1 참조. `enabled: true`와 `local`은 기동 거부. `shared-redis`는 그 자체로 거부된다: 이 빌드는 그 모드의 프로토콜을 싣고 Redis 클라이언트는 싣지 않으므로, 받아들이면 `shared-redis`와 그 공표된 정확도를 답하면서 실제로는 스토어로 조정하는 coordinator를 주게 된다. 역시 정확한 `shared-pg`를 쓸 것 |
| `min_leasable` | int | `16` | `leased` 모드가 노드에 나눌 최소 상한 | 0 이하 거부. `leased`에서 파일 어디든 이 값 미만의 `max_concurrency`는 거부된다 — 한 자릿수 상한은 유용하게 나눌 수 없고, 나눌 수 있는 척하면 대부분의 노드가 블록 0을 쥔 fleet이 된다 |

`leased`에서 검증기는 `providers[].max_concurrency`, `capacity.provider_groups[].max_concurrency`,
`capacity.credential_groups[].max_concurrency`, `capacity.models[].max_concurrency`,
`key_rotation.providers[].keys[].max_concurrency`를 `min_leasable`과 대조한다. 위반마다 경로와 값을 명시한다.

### `node_id`는 모든 노드에서 서로 달라야 한다

node id는 리더 리스의 소유자만이 아니다. `nodes` 레지스트리에서 이 프로세스의 행을 키잉하고 — 그 행의
하트비트가 클러스터에 이 노드가 살아 있음을 알리며, 리더가 죽은 노드의 리스를 회수할 때 걷는 것이 그
행이다 — 리스된 모든 상한에서 이 노드의 몫을 키잉하고, 원장에서 예산 인출을 키잉한다. 같은 id를 가진 두
프로세스는 그 전부에 대해 **하나의 노드**다: 레지스트리 행 하나, 리스 하나, 몫 하나.

그 대가는 설계 §9.2에 적혀 있다: 리더 전용 작업이 전부 두 번 돈다. 보존, 파티션 사전 생성, capacity·예산
예약 sweep, 리스 회수 — 그리고 batch 할당, 두 노드가 같은 batch를 집으면 완료된 행마다 두 번 지불한다.
§1.1이 공표하는 초과는 *살아 있는* 행 수로 계산한 `무언가 × (nodes − 1)`이므로 그것도 틀리며, 보이지
않는 그 노드만큼 정확히 과소 보고된다.

펜싱 토큰은 이것을 잡지 못한다. 토큰은 락의 **보유자**가 바뀔 때 올라가는데, 한 id를 주장하는 두
프로세스는 갱신하는 한 명의 보유자다 — 두 번째 acquire가 갱신으로 받아들여지고, 토큰은 움직이지 않으며,
두 프로세스가 같은 토큰으로 펜스 검사를 통과한다. 토큰은 *term*에 관한 장치이고 이것은 *누구*에 관한
질문이다.

dorang은 대신 레지스트리에서 잡고 **기동을 거부한다**:

```
cluster: another process is already running under this node id: node id "dorang-0" is held by
another process ...
```

- **id가 사용 중임을 발견한 두 번째 프로세스는 합류하지도, 리드하지도, 리더 작업을 돌리지도 않는다.**
- **자리를 비운 사이 대체된 노드** — 긴 GC 정지나 정지된 컨테이너로 하트비트 TTL을 넘겨 얼어붙어 후임이
  정당하게 그 행을 인수한 경우 — 는 다음 하트비트에서 그것을 알아채고, 리드를 멈추고, 물러난 채 남는다.
- **재시작은 중복이 아니다.** 죽은 노드가 남긴 행의 하트비트는 노드 TTL(30초) 안에 만료되고, 재시작은 그
  행을 인수해 자기 리스를 회수한다. 깨끗하게 드레인한 노드는 행을 지웠으므로 대체 프로세스가 즉시 뜬다.
  설정된 id로 *비정상* 재시작할 때만 TTL을 기다리는데, 그 창 동안 클러스터는 여전히 이전 프로세스의 리스를
  그 id에 귀속시키기 때문에 의도된 동작이다.

파일 하나만 보고 증명할 수 있는 두 형태는 로드 시점에 거부된다:

| 값 | 이유 |
|---|---|
| `node_id: "${HOSTNAME}"`, `"{{ .Values.nodeId }}"`, `"node-<ordinal>"` | 치환되지 않은 템플릿은 **모든** 노드에 같은 리터럴 id를 준다. 오케스트레이터에서 치환할 것 |
| `node_id: "dorang-0 "`, `"dorang 0"` | 앞뒤·중간 공백: `"a"`와 `"a "`는 모든 리스에 두 개의 노드이고, 그것을 보고하는 모든 로그 줄에서는 구별되지 않는다 |

구별성에 관한 나머지는 검증기의 능력 밖이다 — 검증기는 언제나 한 노드의 파일만 본다. id가 노드마다
유일함을 보장할 수 없다면 **비워 둘 것**: 파생된 프로세스별 id는 구성상 충돌이 불가능하고, 대가는 재시작
시의 리스 TTL 하나뿐이다.

> **롤링 윈도우는 `local` 모드에서만 정확하다.** 롤링 5시간 허용량에는 자연스러운 period 경계가 없으므로,
> 공유·리스 모드는 그것을 epoch 정렬 그리드에 키잉한다. 실제 근사이며 숨기지 않고 기록한다; 설계는 아직
> 이를 다루지 않는다.

---

## 5. `auth`

```yaml
auth:
  legacy: {enabled: false, until: ""}
  rehash_on_use: true
  miss_budget: {rate: 100, burst: 500}
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `legacy.enabled` | bool | `false` | 마이그레이션 창 동안 unsalted 기존 다이제스트 허용 | §1.3 참조 |
| `legacy.until` | date | `""` | 창의 끝. `legacy.enabled`가 true면 필수 | 파싱 불가하거나 이미 지난 날짜는 기동 거부 |
| `rehash_on_use` | bool | `true` | legacy 검증 성공 시 `dorang_v1`으로의 비동기 업그레이드 예약 | 끄면 마이그레이션이 스스로 끝나지 않고, 창이 닫힐 때 키가 동작을 멈춘다 |
| `miss_budget.rate` | float | `100` | 스토어가 모르는 인덱스 키에 쓸 수 있는 **초당** 스토어 조회 수 | 부정 캐시 항목은 인덱스 키로 키잉되므로, 서로 다른 무작위 키를 내미는 호출자는 절대 그것을 맞히지 못하고 모든 미지의 키가 공짜로 산 DB 왕복 하나였다. 이것이 제한하는 것은 **헛된** 조회뿐이다 — 행을 반환하는 조회는 토큰을 즉시 돌려주므로, 차가운 노드 트래픽의 전부인 진짜 크리덴셜의 첫 사용은 전혀 제한되지 않는다. 배포의 실제 "찾지 못하는 조회" 비율보다 낮게 잡으면 정당한 호출자에게 `503 auth_unavailable`을 답한다: `dorang_auth_lookup_throttled_total`이 오르는데 `dorang_auth_store_calls_total`이 평평해지는지 볼 것. `0`은 미설정이고 기본값이 적용된다. **음수는 상한을 없앤다** — 증폭기를 되살리는 의도적 선택이다 |
| `miss_budget.burst` | int | `500` | 버킷 깊이 — rate가 다스리기 전에 한꺼번에 일어날 수 있는 헛된 조회 수 | 너무 작으면 모든 키에 대해 스토어를 읽어야 하는 차가운 노드가 자기 정당한 트래픽에 스스로 제한된다. `0`은 미설정이고 기본값이 적용된다. **음수는 로드에서 거부**되며 이는 의도적이다: `internal/auth`는 0 이하의 burst를 부재로 읽어 조용히 기본값을 쓸 것이고, 그러면 여기의 마이너스 부호가 한 줄 위의 마이너스 부호와 정반대를 뜻하게 된다. 상한 제거는 `rate`로 한다 |

> **둘 다 동작하는 토큰 버킷을 뒤에 둔 `auth.Config` 필드로 존재했고, 파일에서 오는 경로가 없었다.**
> 모든 배포가 무엇을 쓰든 100/s와 500으로 돌았다 — 아무것도 쓸 수 없었기 때문이다. 그리고 다른 숫자가
> 필요한 배포야말로 내장된 숫자가 아무것도 모르는 배포다: 키를 버스트로 프로비저닝하는 fleet, 또는
> 초당 100번의 헛된 읽기가 이미 과한 만큼 느린 스토어를 가진 배포. 재발 방지 가드가 이 부류를 잡지
> 못한 이유와 지금 무엇이 붙들고 있는지는 §2.1을 볼 것.

두 다이제스트가 항상 계산되고 비교 대상이 branchless하게 선택되므로 키의 스킴이 타이밍으로 관측되지
않는다. 캐시 경로의 인증은 354 ns, 무할당. 암호 연산이 지배하며 스냅샷 조회 자체는 55 ns다.

---

## 6. `providers`

업스트림 서비스 하나. `name`과 `kind`는 필수.

```yaml
providers:
  - name: plan-a
    kind: glm
    base_url: https://api.example-plan-a.invalid
    timeout: 180s
    max_concurrency: 20
    capacity_group: shared-pool
    params:
      drop_unsupported: true
      drop: []
      set: {}
      default: {}
    retry: {max_attempts: 2, backoff: exponential, base: 500ms}
    usage_probe: {enabled: false, fetcher: glm, interval: 60s}
    # `metrics:` 블록은 없다: 아무것도 스크레이프하지 않으므로 로드에서 거부된다. §6.2.
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `name` | string | — | 프로바이더의 정체성. **프로바이더 식별은 여기와 `deployments[].upstream_model`에서만 오고, 모델 문자열 파싱에서 오지 않는다** | 빈 값·중복 거부. 프로바이더를 참조하는 모든 것이 이 이름과 대조된다 |
| `kind` | string | — | 한 단어로 wire adapter, 능력 기본값, 프롬프트 캐시 스킴, 리즈닝 제어 형태를 선택. 모델 카탈로그를 통해 해결되므로 오버레이가 이 빌드가 모르는 kind를 추가할 수 있다 | 빈 값 거부. 알 수 없는 kind는 로드에서 거부되지 **않는다** — 카탈로그 레이어로 해결되며 `dorangctl catalog explain <kind> <model>`이 어느 레이어가 답했는지 알려준다 |
| `base_url` | string | `""` | 업스트림 루트. **적어 넣은 URL에 버전 세그먼트가 없으면 dorang이 붙인다** — §6.0 참고 | 비어 있으면 다이얼할 수 없고, `kind`가 선언한 기본값으로 되돌아간다. `base_url`이 없는 프로바이더의 패스스루 라우트는 아무 데도 가리키지 않게 두는 대신 **조용히 삭제된다** |
| `timeout` | duration | `0` | 프로바이더별 요청 타임아웃. 배포가 override 가능 | 0이면 `server.request_timeout`으로 떨어진다 |
| `max_concurrency` | int | `0` | `route` capacity 축 — 이 프로바이더 자신의 상한 | 음수 거부. `0`은 0짜리 상한이 **아니라** 이 축이 제약하지 않음을 뜻한다 |
| `capacity_group` | string | `""` | 여러 프로바이더가 하나의 업스트림 풀을 공유할 때의 `provider_group` 축 멤버십 | `capacity.provider_groups`에 선언되지 않은 그룹은 이름과 함께 거부 |
| `params.drop_unsupported` | bool | `true` | kind가 표현할 수 없는 파라미터를 전달하지 않고 걸러낸다 | **`true`가 유일하게 유효한 값**이다. 변환 게이트웨이가 가질 수 있는 동작이 그것뿐이기 때문이다 — ⚠️ **`false`는 로드에서 거부된다**(§23.2). 표현할 수 없는 파라미터를 흘려보낼 곳이 없고, 호출자가 손실에 동의하는 경로는 `x-dorang-allow-lossy`다 |
| `params.drop[]` | []string | `[]` | 이 파라미터들을 **인코더가 돌기 전에** 무조건 제거하고, 실제로 지워진 이름을 `x-dorang-dropped-params`로 보고한다 | 빈 항목·공백 패딩 항목은 거부. 무엇을 묻는지 자체를 바꾸는 이름도 거부된다: `model`, `messages`, `system`, `prompt`, `input`, `stream`, `tools`, `tool_choice`, `response_format`, `previous_response_id`, `store`. `anthropic` 형태의 프로바이더에서는 `max_tokens`/`max_completion_tokens`/`max_output_tokens` — 한 필드의 세 철자이며 어느 것을 적어도 같은 필드가 지워진다. Responses 표면 자신의 거부 문구(`Unsupported parameter: max_output_tokens`, Codex에서 측정)를 그 철자 그대로 적을 수 있다 — 도 기동 시 추가로 거부된다. dorang이 모델링하지 않는 이름은 **일부러 받아들인다** — 벤더 고유 노브가 이 목록의 존재 이유다 |
| `params.set{}` | map | `{}` | 호출자를 덮어쓰며 무조건 주입 | ⚠️ 안전 메커니즘을 끌 수 있는 레버다. 여기에 `truncation: auto`나 `truncate_prompt_tokens`를 넣으면 프로바이더 전체에서 context-window 폴백이 키로 삼는 바로 그 `400`이 억제된다 — [EXTENSIONS.ko.md](EXTENSIONS.ko.md) §A.3a |
| `params.default{}` | map | `{}` | 호출자가 보내지 않았을 때만 주입 | `set`보다 안전하지만 같은 두 파라미터로 같은 해를 끼칠 수 있다 |
| `params.force_stream` | bool | `false` | 호출자가 무엇을 요청했든 와이어에 `stream: true`를 보낸다. 다른 것은 거부하는 호스트를 위한 것이다. **호출자가 받는 형태는 바뀌지 않는다**: 비스트리밍 호출자는 여전히 버퍼된 답 하나를 받는다 — dorang이 이벤트 스트림을 읽어 중립 형태로 수집하고, JSON 답과 같은 서빙 모델 검사·도구 인자 검사·§10.5b 변환·클라이언트 인코더를 그대로 거친다. 이 빌드의 카탈로그에서는 ChatGPT Codex 표면(`kind: codex-responses`)만 필요로 한다 | 카탈로그가 `responses_only: true`로 선언한 kind에서**만** 받아들인다. 그 외 kind에서는 키와 프로바이더 이름을 대는 **기동 거부**다: chat 어댑터는 이 키를 읽지 않으므로 로드되고도 아무것도 바꾸지 않을 것이기 때문(§23.2). 수집한 스트림이 종결 이벤트 없이 끝나면 `502 upstream_stream_truncated`, 중간 오류 이벤트는 `502 upstream_stream_error`(업스트림 문구는 `native_message`, 자격증명은 가려짐)이며 둘 다 폴백 체인에 넘기지 않는다 — 호스트는 시작한 생성을 과금한다. 이벤트가 하나도 없는 스트림은 `upstream_shape`로 체인에 넘긴다: 생성된 것이 없다 |
| `params.store_false` | bool | `false` | 와이어에 `store: false`를 무조건 보낸다. 저장 교환을 거부하는 호스트를 위한 것이다. Codex는 `store`가 없거나 true면 `400 Store must be set to false`로 거부한다 | `force_stream`과 같은 규칙: `responses_only` kind에서만 받아들이고 그 외에는 기동 시 거부 |
| `params.api_version` | string | `""` | Azure OpenAI 전용. **레거시** 배포별 표면 — `/openai/deployments/{deployment}/…?api-version=<값>` — 을 선택하며 그 표면의 필수 쿼리 파라미터다. 미지정이면 통합 `/openai/v1` 표면을 쓰고, 배포 이름은 OpenAI 인코더가 넣는 그대로 본문의 `model`로 간다 | `azure`(및 별칭) 외의 kind에서는 기동 시 거부: Azure 어댑터만 읽는다. 자격증명은 두 표면 모두 `api-key` 헤더로 가고, OAuth(Entra ID) 자격증명은 자격증명 테이블을 통해 bearer로 그대로 간다. 실제 리소스에 대해 측정하지는 않았음. Azure의 공개 계약이며 가짜 호스트로 고정 |
| `params.project`, `params.location` | string | `""` | Vertex AI 전용이며 거기서는 둘 다 필수: 경로가 `/v1/projects/{project}/locations/{location}/publishers/google/models/{deployment}:generateContent`이고 어느 쪽도 기본값이 없다. `base_url`은 비워도 된다 — location이 호스트를 정한다(`https://{location}-aiplatform.googleapis.com`, `global`이면 `https://aiplatform.googleapis.com`) | `vertex`(및 `vertex-ai`) 외의 kind에서는 기동 시 거부. 자격증명은 Google OAuth bearer — `auth: oauth`에 `format: gcp-service-account`(아래)로 서비스 계정 키 파일 — 또는 `x-goog-api-key`로 보내는 API 키. Gemini 모델만; `anthropic-vertex`는 이유와 함께 거부. 실제 프로젝트에 대해 측정하지는 않음. Google 공개 경로를 가짜 호스트로 고정 |
| `params.region`, `params.access_key_id`, `params.session_token` | string \| string \| secret ref | `""` | Amazon Bedrock 전용. SigV4 서명 입력: `region`과 `access_key_id`는 필수(`region`은 `base_url` `bedrock-runtime.{region}.amazonaws.com`에서 올 수도 있음), `session_token`은 선택적 단기 STS 토큰이며 다른 비밀처럼 참조(`key_env`/`key_file`/`key_ref`)이지 리터럴이 아니다. **secret access key가 자격증명**(자격증명의 `key_env`)이다 — 비밀인 부분은 그것뿐이고 access key id는 비밀이 아니다 | `bedrock` 외 kind에서는 기동 시 거부. `base_url`은 리전 런타임 호스트로 기본 설정. dorang은 **Converse** API(모델 계열을 아우르는 하나의 요청 형태)를 말하고 SigV4로 서명한다 — 서명기는 AWS 공개 테스트 벡터로 고정, 실계정 측정은 아님 |
| `retry.max_attempts` | int | `2` | 프로바이더 내 재시도, §12의 폴백 체인과 별개 | 음수 거부 |
| `retry.backoff` | `exponential` \| `linear` \| `constant` | `exponential` | 재시도 간격 | 그 외 거부 |
| `retry.base` | duration | `500ms` | 첫 재시도 간격 | 음수 거부 |
| `usage_probe.enabled` | bool | `false` | 프로바이더 자신의 잔여 쿼터 조회 | 없으면 쿼터가 로컬 계측만 되고, 로컬 계측은 dorang을 거치지 않은 트래픽만큼 과소 계산한다 |
| `usage_probe.fetcher` | string | `""` | 어느 프로바이더별 fetcher를 쓸지: `zai`(별칭 `z.ai`·`glm`·`zhipu`·`bigmodel`), `anthropic`, `deepseek` | 프로브가 켜져 있으면 필수. **prober가 없는 fetcher는 기동을 중단시키고** 존재하는 것들을 이름으로 알려 준다 — 스키마가 스스로 검사할 수 없고, 대안은 영원히 아무것도 보고하지 않는 프로바이더다 |
| `usage_probe.interval` | duration | `60s` | 폴 간격이자 한 크리덴셜을 얼마나 자주 읽는지의 하한 | 음수 거부. 폴은 최대 한 간격만큼 낡는다 — §6.1. 간격 안의 두 번째 읽기는 요청을 쓰지 않고 마지막 스냅샷을 재생한다 |
| `usage_probe.allowances[]` | list | `[]` | 프로바이더가 보고한 윈도를 규칙 키에 배치하고 그 크기를 선언한다 | **없으면 퍼센트로 보고하는 프로바이더는 아무것도 게이트하지 못한다** — §6.1a. 어떤 규칙도 쓰지 않는 키에 배치된 윈도는 무력하다 |
| `metrics.*` — 블록 전체 | — | — | **아무것도 하지 않는다. 로드에서 거부된다**(§23.2): §12.4의 백엔드 메트릭 스크레이프에는 이 빌드에 수집기가 없다 | `enabled`·`endpoint`·`interval` 중 무엇이든 쓰면 검증 실패하며, 대신 쓸 것을 이름으로 알려 준다. `least_busy`와 `highest_tps`에는 필요 없다 — §6.2 |

### 6.0 `base_url`의 의미와 dorang이 덧붙이는 것

**벤더가 문서화한 루트를 그대로 적는다. 적어 넣은 URL에 버전 세그먼트가 없으면 dorang이 붙이고,
그 다음에 오퍼레이션 경로를 붙인다.**

| 적어 넣은 값 | dorang이 호출하는 주소 (chat) |
|---|---|
| `https://api.example.com` | `https://api.example.com/v1/chat/completions` |
| `https://api.example.com/v1` | `https://api.example.com/v1/chat/completions` |
| `https://api.example.com/api/anthropic` | `https://api.example.com/api/anthropic/v1/messages` |
| `https://api.example.com/api/coding/paas/v4` | `https://api.example.com/api/coding/paas/v4/chat/completions` |
| `https://api.example.com/v1/openai` | `https://api.example.com/v1/openai/chat/completions` |

"이미 있다"는 것은 경로의 **어디든** 이라는 뜻이지 끝일 필요는 없다 — `v` 뒤에 숫자가 오는
세그먼트(`v1`, `v4`, `v1beta`)다. 마지막 행이 그 이유다. 버전을 패밀리 접두사 앞에 두는 벤더가
있고, 마지막 세그먼트만 보는 규칙은 `…/v1/openai/v1/chat/completions`를 호출하게 된다.

> ⚠️ **세 번째 행이 틀려 있던 자리이고, 조용히 실패했다.** dorang은 베어 호스트에만 버전을
> 붙였으므로, `https://…/api/anthropic`으로 설정한 Anthropic 호환 코딩 플랜은
> `…/api/anthropic/messages`로 호출됐다 — 존재하지 않는 라우트다. 그런 벤더 하나는 서빙하지
> 않는 라우트 요청에 **HTTP 200**과 `{"code":500,"msg":"404 NOT_FOUND"}`로 답하고, dorang은
> 그것을 내용이 빈 성공한 어시스턴트 턴으로 렌더링했다. 양쪽 모두 고쳐졌다. 버전이 붙고,
> 자기 패밀리의 응답이 아닌 업스트림 200은 `502 upstream_shape`이다.
>
> 뒤쪽 절반은 이제 채팅뿐 아니라 **모든** 표면에 적용된다. 임베딩, `count_tokens`, 리랭크,
> 모더레이션, 이미지, 전사, 번역, 음성, Gemini 어댑터 각각이 자기가 서빙하는 형태의 응답이
> 아닌 200을 거부한다. 판정은 페이로드를 담는 멤버의 존재 또는 그 패밀리의 판별자 — 둘 중
> 하나면 되고 둘 다 요구하지 않는다 — 이므로 판별자를 생략하는 벤더도, 진짜로 비어 있는 결과
> 집합도 그대로 통과한다. 음성만 예외인데, 그 응답은 멤버가 없는 오디오 컨테이너라서 판정이
> "비어 있지 않고 JSON 객체도 아닐 것"이 된다.

**벤더가 정말로 버전 없는 라우트를 서빙한다면.** 오퍼레이션 경로로 끝나는 `base_url`은 그대로
쓰인다 — `https://api.example.com/api/gateway/chat/completions`라고 적으면 아무것도 삽입되지
않는다. 오퍼레이션 하나를 고정하는 방식이므로, 하나만 서빙하는 프로바이더에 맞는다.

### 6.1 프로바이더 보고 쿼터를 대체가 아니라 결합하는 이유

프로바이더가 잔여 쿼터를 노출하면 그 값은 dorang을 거치지 않은 트래픽까지 포함해 그 키의 **모든** 사용을
덮는다. 따라서 로컬 계측만으로는 과소 계산이다. 그러나 폴은 최대 한 간격 낡았고, 버스트 중에는 로컬
카운터가 *더 신선한* 신호다. 프로바이더를 "진실"로 선언하고 로컬 델타를 버린 첫 설계는 dorang이 자기
정보가 가장 나쁠 때 가장 공격적으로 라우팅하게 만들었다.

```
effective_used = max( provider_reported_used ,
                      provider_reported_used_at_last_poll + local_delta_since_that_poll )
```

프로바이더 값은 폴마다 재기준화되지만 이전 기준선 + 로컬 델타 아래로는 절대 내려가지 않으므로, 늦은 폴이
버스트를 지울 수 없다. 조회는 프로바이더별 타임아웃과 함께 요청 경로 밖에서 돌고, **실패한 조회는 절대
크리덴셜을 비활성화하지 않는다** — 실패한 읽기는 소진된 쿼터가 아니다. 마지막 정상 스냅샷이 유지되고 낡음
정도가 노출된다.

### 6.1a `allowances` — 없으면 퍼센트는 아무것도 게이트하지 않는다

위의 공식은 단위가 동질적인 산술이고, 프로바이더가 실제로 공표하는 형태는 그렇지 않다. z.ai는
*"5시간 허용량의 37%"*라고 보고하지 *"10,000 중 3,700"*이라고 하지 않는다. Anthropic은 구독 윈도우의
사용률 퍼센트를 보고한다. 퍼센트는 그 메트릭의 단위가 아니고, 거기에 토큰 델타를 더하면 둘 중 어느 것도
아닌 수가 나온다 — 그리고 그 합은 단조인데 롤링 윈도우는 그렇지 않으므로, 설정된 어떤 상한이든 넘어서
자라고 크리덴셜을 아무것도 풀 수 없는 쿨다운에 세워 둔다.

그래서 절대 수치가 없는 윈도우는 **리셋 시각만** 싣는다. 그것이 아무것도 아니지는 않다 — §7.5a(c)의
만료 임박 쿼터 점수는 프로바이더가 언제 리셋되는지 말하기 전까지 롤링 윈도우에 대해 0이므로, 구독
크리덴셜에서 `quota_urgency`가 조금이라도 점수를 내게 만드는 것이 바로 이것이다 — 그러나 어떤 트래픽도
게이트하지 않는다.

`allowances`가 그것을 닫는 방법이다. 그것은 자기 플랜에 대한 오퍼레이터의 단언이며, 절대 추론되지
않는다: 배포의 `tpm`에서 상한을 유도하면 퍼센트를 틀린 분모에 대고 읽는 것이 된다. `tpm`은 그 크리덴셜에
대한 *dorang의* 상한이지 프로바이더의 허용량이 아니기 때문이다.

```yaml
providers:
  - name: plan-a
    kind: zai
    usage_probe:
      enabled: true
      fetcher: zai
      interval: 60s
      allowances:
        # 5시간 윈도우의 "37% 소비"가 12,000,000 토큰 중 4,440,000이 되고,
        # `5h`/`tokens_total` 규칙이 쓰는 키에 배치된다.
        - {label: "tokens_limit:5h", window: 5h, metric: tokens_total, limit: 12000000}
```

| 필드 | |
|---|---|
| `label` | 그 윈도우에 대한 프로바이더 자신의 이름을 internal/probe가 정규화한 형태 — 타입을 소문자로 하고 윈도우 길이로 한정한 것: `tokens_limit:5h`, `five_hour`. 대소문자 무관 매칭 |
| `window` | 규칙 키의 윈도우: 최소 1분 이상의 롤링 duration(`5h`, `1m`), 또는 `daily`/`weekly`/`monthly` |
| `metric` | 규칙 키의 메트릭: `cost_usd`, `tokens_total`, `tokens_input`, `tokens_output`, `requests` |
| `limit` | 허용량의 절대 크기, 그 메트릭의 단위로. 생략하면 크기를 선언하지 않은 채 윈도우를 키에 배치만 한다. 그것만으로도 할 가치가 있다: 리셋 시각을 옳은 규칙에 실어 나르는 것이 그 배치이기 때문이다 |

**프로브가 아무것도 하지 않는 것처럼 보일 때 확인할 것 둘.** 첫째, 그 수치가 게이트하려면 크리덴셜에
규칙이 있어야 한다 — 쿼터는 (윈도우, 메트릭)으로 키잉되고, 규칙은 `metric: rpm` 또는 `tpm`인
`models[].deployments[].limits[]`에서 온다. 게이트할 규칙이 없는 크리덴셜에 프로브를 켜면 시작 시점에
정확히 그 내용이 로그에 남는다. 둘째, 라벨이 일치해야 한다. 어떤 키에도 배치되지 않은 프로바이더 윈도우는
무력하고, 그 침묵이 이 기능의 조용한 실패 모드다.

**발견되기 전에 적어 두는 한계 하나.** 프로브 자신의 호스트를 위한 키는 없으므로, 프로바이더를 사설
호스트명 뒤에 두는 배포도 벤더가 문서화한 엔드포인트를 프로브한다. `providers[].base_url`을 여기에 재사용하지
않는 것은 의도적이다: prober가 있는 모든 프로바이더에서 쿼터 엔드포인트는 다른 서비스이고, 쿼터 읽기를
추론 호스트로 겨누면 404가 나오며 prober의 백오프는 그것을 프로바이더 장애로 취급한다.

### 6.2 백엔드 메트릭 스크레이프는 없고, `least_busy`에는 필요도 없다

**`providers[].metrics`는 로드에서 거부된다.** §12.4는 큐 깊이와 캐시 사용률이 라우팅 입력이 되는
스크레이프를 규정하고 — 그것이 요구사항 R17이다 — 수집기는 끝내 만들어지지 않았다. 블록은 존재하는 내내
검증만 되고 아무 일도 하지 않았다: 플래그가 검사되고, 켜지면 endpoint가 *요구*되고, 설정이 로드되고,
그 URL을 가져오는 코드는 어디에도 없었다.

**§12.4가 이름을 대는 두 전략은 dorang 자신의 측정으로 오늘 동작한다.**

| 전략 | 순위 근거 |
|---|---|
| `least_busy` | 이 요청이 선점할 축에 대한 `internal/capacity`의 실시간 점유율 — 이 게이트웨이가 그 배포에 대해 진행 중인 것의 자기 집계. 정확하고, 낡을 폴 간격이 없다 |
| `highest_tps` | 이 게이트웨이를 통과한 완료 요청에서 측정된 `internal/health`의 초당 출력 토큰 |

둘 다 "아직 표본 없음"을 0이 아니라 *의견 없음*으로 다루므로, 쓰이지 않은 배포가 유리해지지도 불리해지지도
않는다. `models[].strategy`에 이름을 적으면 스크레이프할 것이 없다.

스크레이프가 더해 주는 것은 dorang이 아니라 **엔진 자신의** 시야이고, 그것이 더 나은 신호인 경우는 정확히
하나다: dorang을 거치지 않은 트래픽도 함께 받는 self-hosted 백엔드. 그 경우는 실재하며 R17이 닫을 대상이다.

> **R17이 만들어진다면 이 함정들이 따라온다.** 거부된 키와 함께 잃어버리지 않도록 여기 남긴다. 두
> self-hosted 엔진 모두 잘못 명명됐거나 죽었거나 리셋되는 메트릭이 있다:
>
> - vLLM의 `kv_cache_usage_perc`는 이름과 달리 0–1 분수이고, `--disable-log-stats`를 주면 `/metrics`가
>   **시리즈 0개로 200**을 반환한다 — 유휴와 구별되지 않으면서 정반대를 뜻한다. [VLLM.ko.md](VLLM.ko.md) §3.
> - SGLang의 `/metrics`는 `--enable-metrics`가 없으면 **404**이며 그것이 정직한 실패다. 그리고
>   `sglang:cache_hit_rate`는 매 decode 리포트마다 `0.0`으로 하드 리셋되어 게이지가 대부분의 시간을 0에서
>   보낸다. [SGLANG.ko.md](SGLANG.ko.md) §4.3.
>
> 첫 번째 — 시리즈 없는 200이 유휴로 읽히는 것 — 는 엔진 절을 읽지 않고 출시된 스크레이퍼가 보고를 끄라고
> 지시받은 백엔드 *쪽으로* 라우팅하게 만들었을 것이다. R17의 어려운 부분은 수집기가 아니다.

---

## 7. `credentials`

프로바이더에서의 인증 정체성 하나 — **쿼터와 동시성이 실제로 붙는 단위**이자, 상태를 갖는 대화에 대해서는
대화 상태가 붙는 단위다.

```yaml
credentials:
  - id: plan-a-1
    provider: plan-a
    key_env: DORANG_EXAMPLE_PLAN_A_KEY
    capacity_group: plan-a-account
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `id` | string | — | 크리덴셜의 정체성. **비밀이 아니다**: 라우팅 결정, 메트릭, 헤더, 에러에 나타난다 | 빈 값·중복 거부 |
| `provider` | string | — | 어느 프로바이더에서 인증하는가 | 선언된 프로바이더여야 한다. 다른 프로바이더의 크리덴셜을 열거한 배포는 이름과 함께 거부 |
| `auth` | `key` \| `oauth` | `key` | 이 크리덴셜이 어떻게 인증하는가 | 그 외 값은 거부. `oauth` 블록 없는 `auth: oauth`도, `auth: oauth` 없는 `oauth` 블록도 거부 |
| `key_env` / `key_file` / `key` | secret ref | — | 비밀값 출처 (§0.3) | 0개 또는 2개 이상 거부. `key_ref`는 그 자체로 거부 |
| `oauth` | 블록 | — | §7.2. `auth: oauth`에서만 | 키와 `oauth` 블록을 **둘 다** 세운 크리덴셜은 거부된다: 둘은 택일이며, 우선순위로 푸는 것이야말로 파일에 아무 설명도 없이 잘못된 크리덴셜을 보내는 방법이다 |
| `capacity_group` | string | `""` | `credential_group` 축 멤버십 — 계정당, 그 계정이 서빙하는 모든 모델에 걸쳐 | `capacity.credential_groups`에 선언되지 않은 그룹 거부 |

> **키 자료를 두 곳에 선언할 수 있고 스키마는 어느 쪽이 이기는지 말하지 않는다.** 같은 크리덴셜 id가
> `credentials[]`와 `key_rotation.providers[].keys[]` 양쪽에 비밀 참조를 실을 수 있다. 둘은 하나를 고르는
> 대신 병합되며, 그것이 예시 설정의 분리 — 한쪽에 프로바이더 바인딩, 다른 쪽에 동시성 상한 — 를 말 그대로
> 만든다. 둘이 *다른* 비밀값을 실으면 해결 순서는 미명세다. 비밀값은 한 번만 선언할 것.

### 7.2 `oauth` — 키가 아니라 토큰으로 인증하는 계정

어떤 프로바이더는 API 키가 아니라 구독을 판다. 그때 인증하는 것은 한 시간이 채 안 되어 만료되는 OAuth
액세스 토큰이다. 그 토큰은 벤더의 CLI를 돌리는 기계라면 이미 디스크에 있으므로, 크리덴셜은 서버에서 두
번째 인가 플로를 시작하는 대신 **그 CLI의 저장소를 가리킨다**.

```yaml
credentials:
  - id: plan-oauth-1
    provider: plan-a
    auth: oauth
    oauth:
      source: file                          # file | exec | env
      path: /path/to/the/vendor/auth.json   # CLI 자신의 저장소. 앞의 ~/ 는 확장된다
      format: codex                         # generic | codex | claude | gemini
      account_header: chatgpt-account-id    # 프로바이더가 계정 id를 원하는 헤더
      refresh_margin: 5m
      refresh:                              # 선택 — 아래 참조
        token_url: https://auth.example.com/oauth/token
        client_id: the-cli-s-published-client-id
        key_env: DORANG_PLAN_A_CLIENT_SECRET   # 공개(PKCE) 클라이언트면 생략
        encoding: form                      # form | json
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `source` | `file` \| `exec` \| `env` | `file` | 토큰을 어디서 읽는가. dorang이 되쓸 수 있는 것은 `file` 뿐 | 모르는 source 거부. `path` 없는 `file`, `command` 없는 `exec`, `env_var` 없는 `env`는 각각 이름과 함께 거부 |
| `path` | 경로 | — | `source: file`의 저장소. **참조**일 뿐이다: 토큰이 설정 파일에 들어가는 일은 없다 | 시작 시 읽히지 않으면 게이트웨이를 죽이는 대신 그 크리덴셜만 unhealthy로 표시된다 — 한 계정의 없는 파일이 다른 계정까지 끌어내려서는 안 된다 |
| `command` | 리스트 | — | `source: exec`의 argv | 읽기 전용: 토큰의 주인은 그 명령이다 |
| `env_var` | string | — | `source: env`의 변수 이름 | 읽기 전용: 토큰의 주인은 환경이다 |
| `format` | `generic` \| `codex` \| `claude` \| `gemini` | `generic` | 저장소의 키 배치 | 모르는 포맷은 이 빌드가 아는 것들을 열거하며 거부 |
| `access_token_field` / `refresh_token_field` / `expires_at_field` / `account_id_field` | string | 포맷의 값 | 포맷의 키 이름을 덮어쓴다. `tokens.access_token` 같은 점 표기 경로는 중첩 객체 안까지 닿는다 | 아무것도 가리키지 않는 이름은 에러가 아니라 저장소에 그 필드가 없다는 뜻이다 |
| `account_header` | 헤더 이름 | `""` | 토큰과 함께 계정 id를 실어 보낼 헤더 | 비우면 보내지 않는다 |
| `refresh_margin` | duration | `5m` | 만료 얼마 전에 갱신하는가 | 토큰이 죽은 뒤에야 일어나는 갱신은 메커니즘이 아니라 401 폴백이다 |
| `poll_interval` | duration | `30s` | 루프가 시계를 보는 주기. `refresh_margin`의 1/4로 클램프된다 | 마진보다 느린 폴은 창 전체를 건너뛴다 |
| `exec_timeout` | duration | `10s` | `source: exec` 명령의 시간 제한 | — |
| `refresh.token_url` | https URL | — | RFC 6749 토큰 엔드포인트. **이걸 세우는 것이 갱신을 켜는 행위다** | 평문 `http`는 거부: 갱신 요청의 본문이 곧 리프레시 토큰이다 |
| `refresh.client_id` | string | — | OAuth 클라이언트. `token_url`이 있으면 필수 | — |
| `refresh.key_env` / `key_file` / `key` | secret ref | — | 클라이언트 시크릿. 다른 모든 비밀값과 같은 철자다 (§0.3). 공개 클라이언트면 생략 | `token_url` 없이 세우면 거부 |
| `refresh.scope` | string | `""` | 비어 있지 않으면 함께 보낸다 | — |
| `refresh.encoding` | `form` \| `json` | `form` | 요청 본문의 철자. 대부분 `form`, 일부 벤더는 `json`을 요구한다 | 모르는 encoding 거부 |
| `refresh.timeout` | duration | `30s` | 교환 한 번의 시간 제한 | — |

**갱신은 옵트인이고, 기본값이 안전한 쪽이다.** `refresh` 블록이 없으면 dorang은 저장소를 읽어 벤더의 CLI가
마지막으로 써 둔 것을 채택할 뿐 **아무것도 쓰지 않는다**. 리프레시 토큰은 운영자가 어디에 쓸지 말한 곳에서만
소비된다. `refresh` 블록이 있으면 갱신은 만료 전에 백그라운드 루프에서 일어나고, 교환은 크리덴셜당
single-flight이며, 후속 토큰은 dorang이 소유하지 않은 모든 키를 보존한 채 원자적으로 되쓰인다 — 그 파일의
주인은 CLI이고, 그 키 하나를 떨구는 쓰기는 아무도 게이트웨이 탓이라고 생각하지 않을 방식으로 CLI를 망가뜨린다.

**dorang은 크리덴셜을 갱신하지 발급하지 않는다.** *첫* 리프레시 토큰을 얻는 일은 dorang이 호스팅할 곳 없는
리다이렉트를 가진, 브라우저에 묶인 대화형 플로다. 벤더의 CLI로 로그인한 뒤, 그것이 쓴 저장소를 가리켜라.

**만료 필드가 없는 저장소.** 지원하는 배치 중 하나는 만료를 아예 싣지 않는데, 만료가 없는 토큰은 미리 갱신되지
않으므로 기능 전체가 401 폴백에 얹히게 된다. 그 배치에서는 액세스 토큰 자신의 JWT `exp` 클레임에서 만료를
읽는다. 클레임은 읽을 뿐 검증하지 않는다: dorang은 audience가 아니고 키도 갖고 있지 않다.

**크리덴셜 집합은 핫 리로드하지 않는다.** 각각이 시작 시 세워진 토큰·백오프·백그라운드 루프를 쥐고 있어서,
OAuth 크리덴셜을 추가·삭제·이동하는 리로드는 재시작하라는 메시지와 함께 **거부**된다. 파일의 나머지는 그대로
리로드된다. §0.5에 리로드하지 않는 다른 절이 있다.

**토큰은 비밀값이다 (§0.3, DESIGN §4.1).** 이 서브시스템을 떠나는 것 중 토큰을 실을 수 있는 것은 없다: id와
health가 전부다. 설정 파일에 있는 것은 *경로*와 *클라이언트 id*이지 토큰이 아니다.

### 7.1 크리덴셜 어피니티는 캐시 최적화가 아니라 정확성 제약이다

한 프로바이더에 여러 계정은 정상 사례다. 놓치기 쉬운 것은 상태를 갖는 API에서 크리덴셜이 **대화 상태**가
붙는 단위이기도 하다는 점이다. 계정 범위인 것 넷:

| 계정 범위 상태 | 다음 턴이 다른 곳에 착지하면 |
|---|---|
| 서버측 응답 핸들(`previous_response_id` 등) | 핸들이 해결되지 않는다 — 하드 에러, 더 나쁘게는 잘린 대화에서 나온 그럴듯한 답 |
| 무결성 보호된 리즈닝 블록 | 받는 계정이 자기가 발행하지 않은 블록을 검증할 수 없다 |
| 프롬프트 캐시 잔존 | 조용한 비용·지연 회귀; 아무것도 보고하지 않는다 |
| 쿼터·지출 윈도우 | 실패가 아니라 — 풀이 존재하는 이유다 |

셋째만 복구 가능하다. 그래서 어피니티에 강도가 둘 있고, 잘못 고르는 것이 버그다: **preferred**는 capacity
압박에서 다른 계정으로 spill하며 무상태 chat에 옳고, **pinned**는 기다리거나 실패한다 — 다른 계정은 더
나쁜 선택이 아니라 틀린 선택이기 때문이다.

dorang은 핀을 설정에서 신뢰하는 대신 **추론한다.** 서버측 핸들이나 opaque 리즈닝 블록을 실은 요청은
`key_rotation.…stickiness`가 뭐라 하든 그 사실로 고정된다. 설정은 비용에 대한 선호를 표현하는데 이것은
비용에 관한 질문이 아니기 때문이다. 설정은 *고정되지 않은* 경우의 정책만 고른다. 설계의
`stickiness.pin_on_state` 노브가 스키마에 없는 이유가 그것이다.

---

## 8. `capacity`

다축 상한. 한 프로바이더 안에서도 동시성 상한이 세는 단위가 다르다 — 어떤 계정은 모든 모델에 걸쳐 3개를
허용하고, 코딩 플랜은 *모델별* 7개를 허용해 모델 두 개면 한 키에 동시 14개다. 축은 둘을 동시에 표현하기
위해 존재한다.

```yaml
capacity:
  provider_groups:   {shared-pool: {max_concurrency: 24}}
  credential_groups: {plan-a-account: {max_concurrency: 7}, acct-1: {max_concurrency: 3}}
  models:
    - {provider: plan-a, model: model-x, max_concurrency: 7}
    - {provider: plan-a, model: model-y, max_concurrency: 7}
  principals: {default: {max_concurrent: 32, max_queue_wait: 30s}}
  global: {max_concurrency: 256}
  interactive_reserve: 0.3
```

### 8.1 축

| 축 | 설정 위치 | 세는 단위 |
|---|---|---|
| `route` | `providers[].max_concurrency` | 프로바이더 하나 |
| `provider_group` | `capacity.provider_groups` | 업스트림 풀을 공유하는 여러 프로바이더 |
| `model` | `capacity.models[]` | **(프로바이더, upstream 모델) 쌍 하나** — 그 프로바이더의 모든 credential이 공유한다 |
| `credential_group` | `capacity.credential_groups` | **계정 하나, 모든 모델** |
| `key` | `key_rotation.providers[].keys[].max_concurrency` | 키 하나 |
| `principal` | `capacity.principals` | 호출자 하나, **API 키 id**로 키잉 |
| `global` | `capacity.global` | 프로세스(공유 모드에서는 클러스터) |

요청이 필요로 하는 모든 축이 **하나의 임계 구역**에서 전량 또는 전무로 예약된다. 부분 점유가 존재하지
않으므로 대기자가 한 슬롯을 쥔 채 다른 슬롯을 기다리는 일이 없고, 데드락이 구조적으로 불가능하다. 대기는
축별 FIFO에 타깃 wakeup이며, 상한 1·7·32에 대해 대기자 1000명에서 **grant당 정확히 1.00 wakeup**으로
측정됐다. broadcast라면 release당 `O(대기자)`다. 전체 7축 acquire 경로는 455 ns, 할당 1회.

**"부분 보유 금지"의 대가는 liveness 구멍이었고, 닫혔다.** 포화된 두 축이 필요한 대기자가 두 큐의
head에 앉은 채 둘이 동시에 비는 순간을 영영 만나지 못할 수 있었다. 데드락도 추월 문제도 아니므로 aging
메커니즘은 닫을 수 없었다. 리스크 W8로 기록돼 있고 **정본 상태는 DESIGN §18**이다: 소프트 예약으로
해결 — 축 키당 한 단위에 대한 prefix 순서의 청구권이며, 데드락은 §5.7의 축 순서로 배제된다. 가장 오래
기다린 대기자는 `SoftReserveAfter + 축 수`번의 release 안에 서빙된다. 혼합 경합 벤치마크에서 +5.3%,
단일 축에서는 0이었다.

**이것을 위한 키는 없고, 이 문서가 그것에 대해 할 말은 그게 전부다.** 가드는 켜져 있고 실패한 probe
4회 뒤에 arm한다. `capacity.Config.SoftReservations`와 `.SoftReserveAfter`는 임베더가 설정할 수 있는
Go 옵션이지만 `internal/app`은 둘 다 설정하지 않으므로 어떤 YAML도 거기에 닿지 않는다. 기본값이 곧
동작이고, 다른 동작을 원하는 오퍼레이터에게 그것을 위한 설정은 없다 — 키가 무력해질 수조차 없이 아예
존재하지 않으므로 §23.1이 아니라 여기에 적는다.

> **이 문단은 이번 패스 전까지 반대를 말했다.** *"아직 열려 있는 liveness 구멍 … 아직 설계되지 않은
> 소프트 예약 프로토콜이 필요하다"*라고 적혀 있었고, 그동안 DESIGN §18은 W8을 닫힘으로 기록했으며
> `internal/capacity/softreserve.go`가 `039c0b6`에서 그 프로토콜을 이미 배로 실었다. 영문 §8.1도
> 같은 문장을 들고 있었으니 이것은 미러의 드리프트가 아니라 양쪽 모두의 오류였다.

### 8.2 키

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `provider_groups.<name>.max_concurrency` | int | `0` | provider-group 상한 | 음수 거부. **`0`은 0짜리 상한이 아니라 이 축이 제약하지 않음을 뜻한다.** 프로바이더가 참조하는데 여기 선언되지 않은 그룹은 거부 |
| `credential_groups.<name>.max_concurrency` | int | `0` | 모든 모델에 걸친 계정당 상한 | 동일. "모델 무관 동시 3개"를 모델링하는 축 |
| `models[].provider` | string | — | 상한이 속한 프로바이더. 축 키의 절반이며, 나머지 절반은 credential이 **아니다** | 선언돼 있어야 한다 |
| `models[].model` | string | — | 상한이 세는 **업스트림** 모델 이름 | 빈 값 거부 |
| `models[].max_concurrency` | int | `0` | (프로바이더, upstream 모델)당 상한 | **키당이 아니라 프로바이더당이다**: 같은 프로바이더의 두 번째 credential은 같은 버킷에서 가져간다. 또한 `credential_groups`와 겹쳐 적용되고 더 좁은 쪽이 이긴다 — 계정당 7로 묶인 플랜은 이 값을 어떻게 두든 모델 둘을 각각 7씩 돌리지 못한다 |
| `principals.<id>.max_concurrent` | int | `default`에 대해 `32` | 호출자별 상한. `<id>`는 **API 키 id**이고, `default` 항목이 자기 항목 없는 모든 호출자에 적용된다 | 음수 거부. `default` 항목은 항상 존재한다 — 쓰지 않으면 `{max_concurrent: 32}`가 삽입된다 |
| `global.max_concurrency` | int | 미설정 | 프로세스 또는 클러스터 전역 상한 | 부재는 전역 상한 없음. `global:` 블록 전체를 생략하는 것과 `{max_concurrency: 0}`이라고 쓰는 것은 의도에서만 다르고, 둘 다 축을 제약하지 않는다 |
| `interactive_reserve` | float | `0.3` | batch가 점유할 수 없는 **모든** 축의 비율 | `[0,1)` 이내여야 한다. `1.0` 이상은 거부 — 모든 축을 batch가 전혀 쓸 수 없게 만드는데, 그것은 `0.999…`로 표현 가능하고 실제로는 오타일 가능성이 더 크다 |

⚠️ **`max_concurrency` / `max_concurrent`만 강제된다.** 스키마는 모든 capacity 그룹에 `rpm`, `tpm`,
`max_queue`를, principal에 `rpm`, `tpm`, `max_queue`, `max_queue_wait`를 받는다. 검증기는 부호만 확인하고
그다음 **아무것도 읽지 않는다**: capacity broker는 세는 게이지만 구현하며 토큰 버킷도 큐 깊이 상한도 없다.
§23.1 참조. 설정했다고 믿는데 아무 일도 하지 않는 상한은 상한이 없는 것보다 나쁘므로, 이 문서에서 가장
중요한 행이다.

### 8.3 `interactive_reserve`가 축별인 이유와 산술 함정 둘

초기 설계는 batch를 *크리덴셜* 동시성의 일부로 제한했다. 그것은 *모델* 축을 보호하지 못한다: batch가
크리덴셜 몫 안에 머물면서 한 모델의 상한 전부를 가져가 그 모델의 인터랙티브 트래픽을 완전히 굶길 수 있다.
그래서 예비는 **모든** 축에 적용된다.

산술이 요구한 것 둘:

- `floor(10 × (1 − 0.3))`은 이진 부동소수에서 7이 아니라 **6**이라, 예비가 설정값을 조용히 초과한다.
  엡실론이 적용된다.
- `floor(1 × 0.7) = 0`은 축을 단지 경합이 아니라 *충족 불가능*으로 만든다. 영원히 블로킹하는 대신 즉시
  에러를 반환한다. 상한 1과 예비를 함께 설정하면 그 축의 batch 몫은 0이고 거기서 batch는 거부된다 —
  옳은 동작이지만 대기가 아니라 거부다.

엄격 FIFO와 예비도 충돌한다: 큐 head에서 막힌 batch 대기자가 예비가 보호하려는 바로 그 인터랙티브
트래픽을 head-of-line 블로킹한다. 다시 막힌 head 대기자는 시퀀스 번호를 유지한 채 그 패스 동안 주차되어
다음으로 오래된 대기자에게 차례가 간다.

---

## 9. `key_rotation`

```yaml
key_rotation:
  strategy: least_used
  providers:
    cloud-a:
      affinity_group: cloud-a-accounts
      stickiness: {scope: session, on_capacity: spill}
      keys:
        - {id: acct-1, key_env: KEY_1, max_concurrency: 3, capacity_group: acct-1}
        - {id: acct-2, key_file: /etc/dorang/secrets/2.key, max_concurrency: 3, capacity_group: acct-2}
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `strategy` | `round_robin` \| `least_used` \| `failover` \| `random` | rotation이 설정되면 `least_used`, 아니면 미설정 | 키 풀 순서를 정한다 | 그 외 거부. 네 이름 모두 선호 크리덴셜을 고르며(`internal/app/build.go`의 `router.ParseRotation`), 크리덴셜 핀과 sticky 항목이 여전히 rotation보다 우선한다. §23.1a |
| `providers.<name>` | map | — | 한 프로바이더의 키에 대한 rotation 정책 | 그 프로바이더가 선언돼 있어야 한다 |
| `providers.<name>.affinity_group` | string | `""` | 풀 라벨 | — |
| `providers.<name>.stickiness.scope` | `session` \| `api_key` \| `user` \| `team` \| `tenant` \| `none` | `""` | 크리덴셜 핀이 키잉되는 범위 | 그 외 거부 |
| `providers.<name>.stickiness.on_capacity` | `wait` \| `spill` | `""` | **고정되지 않은** 요청이 선호 크리덴셜 포화 시 하는 일 | 그 외 거부. 고정된 요청에는 **절대** 적용되지 않는다 (§7.1) |
| `providers.<name>.keys[].id` | string | — | 이 키의 상한이 세는 크리덴셜 id | 빈 값·풀 내 중복 거부. *다른* 프로바이더의 크리덴셜을 가리키면 이름과 함께 거부 |
| `providers.<name>.keys[].key_env` 등 | secret ref | — | 키 자료 (§0.3) | 같은 규칙 |
| `providers.<name>.keys[].max_concurrency` | int | `0` | `key` capacity 축 | 음수 거부; `leased`에서 `cluster.min_leasable` 미만 거부 |
| `providers.<name>.keys[].capacity_group` | string | `""` | credential-group 멤버십 | `capacity.credential_groups`에 선언돼 있어야 한다 |

---

## 10. `models`, `aliases`, `classes`

```yaml
models:
  - name: model-x
    class: chat-large
    strategy: [prefix_sticky, lowest_cost, least_busy]
    deployments:
      - provider: plan-a
        upstream_model: model-x
        credentials: [plan-a-1]
        weight: 10
        priority: 0
      - provider: cloud-a
        upstream_model: model-x:cloud
        credentials: [acct-1, acct-2]
        weight: 5
        priority: 1
        timeout: 90s
        stream_timeout: 60s
        limits:
          - {metric: rpm, value: 600}
          - {metric: tpm, value: 400000}

aliases: {model-large: model-x}
classes: {chat-large: [model-x]}
```

### 10.1 모델 이름은 불투명하다

**어떤 컴포넌트도 모델 이름을 어떤 문자로도 쪼개지 않는다.** 실제 이름은 `:`를 최소 세 가지로 쓴다 —
패밀리 태그, 벤더 프리픽스, 배포 변종. 한 코드 경로에서 쪼개고 다른 경로에서 보존하는 게이트웨이는 같은
모델에 대해 식별·라우팅·alias·prefix 어피니티를 전부 비결정적으로 만든다. 프로바이더 식별은
`providers[].name`과 `deployments[].upstream_model`에서만 온다.

설정에 대한 귀결: `model-x:cloud`는 구조가 아니라 이름이고, dorang은 거기서 프로바이더를 추론하지 않는다.

### 10.2 키

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `models[].name` | string | — | 클라이언트 대면 이름. **통째로** 비교 | 빈 값·중복 거부. alias이기도 한 이름은 거부 — 같은 이름이 두 가지를 뜻할 수 없다 |
| `models[].class` | string | `""` | 모델 클래스, fail-back이 위임할 수 있는 범위 | `classes`에 선언되지 않은 클래스는 거부. 클래스를 선언했는데 **그 클래스의 멤버 목록에 없는** 모델도 거부: 양방향이 일치해야 한다 |
| `models[].strategy[]` | []string | `[prefix_sticky, lowest_cost, least_busy]` | tie-break 체인. 허용: `round_robin`, `least_busy`, `lowest_cost`, `lowest_latency`, `highest_tps`, `sticky`, `prefix_sticky`, `priority`, `weighted_random`, `quota_urgency` | 그 외 거부. `quota_urgency`는 기본적으로 `lowest_cost` **뒤에** 합성된다(§7.5a(c)): 만료 임박 허용량을 선호하는 것은 대안 역시 이미 지불된 경우에만 옳다. 기본 체인은 "따뜻하게, 그다음 싸게, 그다음 한가하게" |
| `models[].deployments[]` | list | — | 라우팅 후보, 설정 순서대로 — 그 순서가 최종 tie-break이므로 map 무작위가 아니라 결정적 | 배포가 없는 모델은 거부 |
| `deployments[].provider` | string | — | 어느 프로바이더 | 선언돼 있어야 한다 |
| `deployments[].upstream_model` | string | — | 업스트림으로 보내는 진짜 모델 id. **그대로** 쓰이며 절대 파싱되지 않는다 | 빈 값 거부 |
| `deployments[].credentials[]` | []string | `[]` | 이 배포를 서빙할 수 있는 계정들, 선호 순 | 각각 선언돼 있어야 하고 이 배포의 프로바이더 소속이어야 한다 |
| `deployments[].weight` | int | `0` | `round_robin`과 `weighted_random`에 편향. 0은 1로 취급 | 음수 거부 |
| `deployments[].priority` | int | `0` | `priority` 전략의 순서. **낮을수록 선호** | 음수 거부 |
| `deployments[].timeout` | duration | `0` | 이 배포의 요청 타임아웃. capacity 예약도 제한 | 음수 거부 |
| `deployments[].stream_timeout` | duration | `0` | 스트리밍 유휴 타임아웃 | 음수 거부 |
| `deployments[].limits[]` | `{metric, value}` 리스트 | `[]` | 메트릭 이름: `max_concurrent`, `rpm`, `tpm`, `max_queue` | 알 수 없는 메트릭이나 음수 거부. ⚠️ **`rpm`과 `tpm`만 작동**하며, 배포가 아니라 *크리덴셜*에 대한 롤링-분 쿼터가 된다 |
| `aliases.<name>` | string | — | 가상 이름 → 모델 그룹. 업스트림은 진짜 id를 받고, 응답 본문은 클라이언트가 요청한 이름을 되돌려준다 | 모델 이름이기도 한 alias 거부. 대상이 또 다른 alias면 거부: **alias는 체인하지 않는다** |
| `classes.<name>[]` | []string | — | 교체 가능한 그룹 | 멤버 없는 클래스 거부; 중복 멤버 거부; 선언되지 않은 모델 거부 |

> **배포의 `rpm`/`tpm`은 크리덴셜별 쿼터가 되고, 두 배포를 서빙하는 크리덴셜은 둘 중 더 엄격한 것을
> 취한다.** 파일은 상한을 배포에 진술하고, 설계 §3은 쿼터를 크리덴셜에 붙인다. 더 엄격한 쪽을 취해
> 해소하는 것은 의도적이다: 반대 방향으로 틀리면 설정된 상한이 조용히 존재를 멈춘다. 한 크리덴셜의 두
> 배포가 서로 다른 rate를 필요로 하면 크리덴셜을 나눌 것.

### 10.3 응답이 alias를 되돌려주는 이유

업스트림은 항상 진짜 모델 id를 받는다. 응답 본문의 `model` 필드는 **클라이언트가 요청한 이름**을
되돌려주며, 모든 스트리밍 청크마다 다시 찍힌다. 그것은 피할 최적화가 아니다 — 기존 클라이언트들이 만들어진
대상 프록시가 청크마다 다시 찍고, `model`에 고정된 클라이언트는 그러지 않으면 깨진다.

메커니즘은 JSON 디코드가 아니라 SSE 프레임 위의 단일 패스 라인 지향 스캐너이며, dorang은 업스트림에
`Accept-Encoding: identity`를 요청해 패치할 압축 바이트가 없게 한다. 요청 이름과 업스트림 이름이 같으면
스캐너는 평범한 복사로 단락한다. 이전의 "첫 프레임을 바이트 오프셋으로 패치" 주장은 철회됐다: 정상
사례에서 길이가 다르고, 프레임 경계가 임의이며, 필드가 첫 프레임에 안정적으로 있지 않고, 압축이 아예
불가능하게 만든다.

---

## 11. `routing`

```yaml
routing:
  sticky: {enabled: true, ttl: 1h, purge_interval: 5m, key: [api_key, session_id]}
  prefix: {enabled: true, chunk_bytes: 4096, checkpoints: logarithmic, max_bytes: 64MiB, ttl: 1h}
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `sticky.enabled` | bool | `true` | 업스트림 캐시 수명 동안 세션을 대상에 고정 | — |
| `sticky.ttl` | duration | `1h` | 핀이 **생성 시점부터** 사는 시간 | sticky가 켜져 있으면 0 이하 거부. 사용 시 갱신되지 **않는다**: 전제가 TTL 이후 업스트림 캐시가 사라졌다는 것이므로 갱신은 요점을 무너뜨린다 |
| `sticky.purge_interval` | duration | `5m` | 만료 핀 정리 주기 | 음수 거부 |
| `sticky.key[]` | []string | `[api_key, session_id]` | 핀 키 구성 요소. 허용: `api_key`, `session_id`, `user`, `team`, `tenant` | 빈 목록이나 알 수 없는 요소 거부. tenant 요소가 내부 키의 선두이므로 두 테넌트가 핀을 공유하지 않는다 |
| `prefix.enabled` | bool | `true` | 해시 체인에 의한 순서 정확 prefix 어피니티 | — |
| `prefix.chunk_bytes` | size | `4096` | 기본 세그먼트 크기. **바이트 경계 — 핫패스에 토크나이저가 없다** | 0 이하 거부 |
| `prefix.checkpoints` | `logarithmic` \| `fixed` | `logarithmic` | 깊이 선택 방식 | 그 외 거부. `fixed`는 시스템 프롬프트를 공유하고 이후 갈라지는 두 대화를 구별하지 못한다 — 가장 깊은 추적 노드에서 충돌하고 "가장 긴 공통 prefix 우선"이 조용히 거짓이 된다 |
| `prefix.max_bytes` | size | `64MiB` | 테이블은 엔트리 수가 아니라 **보유 바이트**로 예산 | 0 이하 거부. 요청 하나가 모든 checkpoint에 엔트리를 쓰고, 엔트리당 크기가 과소 추정돼 있었다; 배포 id를 interning해 엔트리를 24바이트로 유지한다 |
| `prefix.ttl` | duration \| `until_evicted` | `1h` | 엔트리 **기본** 수명, 프로바이더별·배포별로 override된다. 세션 핀과 달리 prefix 엔트리는 **사용 시 갱신된다** — 여전히 쓰이는 prefix는 업스트림 캐시를 따뜻하게 유지하고 있다 | prefix가 켜져 있으면 0 이하 거부; `until_evicted`는 허용된다 |

#### 어피니티 수명은 백엔드에 대한 사실이다

`prefix.ttl`이 모델링하는 것은 하나다: **백엔드가 이 prefix의 KV 블록을 아직 붙들고 있는 시간.** 그것은
dorang의 성질이 아니고, 벤더 사이에서 한 자릿수 배 이상 차이가 난다.

| 백엔드 | 실제 동작 | 적을 값 |
|---|---|---|
| Anthropic 프롬프트 캐싱 | 기본 티어 ~5분, 확장 티어 1시간 — 그리고 티어는 **요청**별 성질이다 | 배포별로 `5m` 또는 `1h` |
| OpenAI 자동 프롬프트 캐싱 | 대략 5–10분, 계약된 값 아님 | `5m` |
| Gemini 명시적 캐싱 | 캐시된 콘텐츠 자체에 오퍼레이터가 설정한 TTL | 거기 설정한 값 |
| vLLM, SGLang | **TTL 자체가 없다** — 블록은 메모리 압박에서 LRU로 쫓겨날 때까지 산다 | `until_evicted` |

틀렸을 때의 대가는 대칭이 아니다. 너무 길면 라우터가 더 이상 그 prefix를 갖고 있지 않은 노드에 대화를
계속 고정한다: 이득 없는 sticky 라우팅이고, 잃은 로드밸런스와 뜨거운 노드로 지불한다. 너무 짧으면 있었을
히트를 버린다.

그래서 수명은 세 층위에서 설정 가능하고, 더 구체적인 쪽이 이긴다:

```yaml
routing:
  prefix: {ttl: 30m}                     # 기본값

providers:
  - {name: fleet-vllm, kind: vllm, prefix_ttl: until_evicted}
  - {name: cloud-a,    kind: anthropic, prefix_ttl: 5m}

models:
  - name: model-x
    deployments:
      - {provider: cloud-a, upstream_model: …, prefix_ttl: 1h}   # 확장 캐시 티어
```

`until_evicted`는 엔트리가 시계로는 만료되지 않는다는 뜻이다. 그래도 상한은 있다: 어피니티 테이블은
**바이트**로 예산되고(`prefix.max_bytes`), 축출은 두 패스로 돈다 — 만료된 엔트리 먼저, 그다음 마지막
사용이 가장 오래된 것. 수명 없는 엔트리는 그저 첫 패스에 들어가지 않을 뿐이고, 그것이 self-hosted 엔진의
prefix 캐시가 갖는 계약 그대로다.

체인은 `h₀ = H(group)`, `hᵢ = H(hᵢ₋₁ ‖ cᵢ ‖ len(cᵢ))`다. 깊이 *i*에서의 일치는 `c₁..cᵢ`가 바이트 단위로
동일하고 **그 순서 그대로**임을 증명한다. 길이가 prefix가 아니라 *suffix*인 것이 체인을 완전히 스트리밍
가능하게 만든다: 후행 부분 세그먼트의 길이는 끝나야 알 수 있으므로, 앞에 두면 본문을 버퍼링해야 했다.
16 MiB가 2.4 GB/s에서 728 B/op; 4 KiB 요청은 2 µs; 히트 시 조회는 110 ns, 무할당.

세그먼트는 기하급수적으로 자란다. 16 MiB 본문은 13개 다이제스트를 낳고, 고정 4 KiB 청킹이면 4096개다.

⚠️ **프롬프트 머리 근처의 가변 토큰은 prefix 재사용을 그 앞의 것까지로 붕괴시킨다.** 어떤 클라이언트는 매
턴 해시가 바뀌는 요청별 attribution 블록을 앞에 붙인다. 엔진 고유 문제가 아니고 dorang이 밖에서 고칠 수도
없다 — 그러나 dorang 역시 전달하는 프롬프트 앞에 요청 범위 메타데이터를 붙여 그런 것을 만들어내서는
절대 안 된다.

---

## 12. `fallbacks`

```yaml
fallbacks:
  on:
    rate_limit:      [same_group, same_class]
    quota_exhausted: [same_group, same_class]
    context_window:  [same_class_larger]
    content_policy:  [same_class]
    upstream_5xx:    [same_group, same_class]
    timeout:         [same_group]
    budget_exceeded: []
    auth:            []
  max_hops: 3
  budget_ms: 120000
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `on.<cause>` | []string | 위 표 | 원인별 체인. 원인: `rate_limit`, `quota_exhausted`, `context_window`, `content_policy`, `upstream_5xx`, `timeout`, `budget_exceeded`, `auth`. 대상: `same_group`, `same_class`, `same_class_larger` | 알 수 없는 원인·대상 거부. **`budget_exceeded`와 `auth`는 빈 체인이어야 한다** — 비어 있지 않으면 거부 |
| `max_hops` | int | `3` | **첫 디스패치 이후의** 폴백 시도 수 제한, 즉 `3`은 **총 네 번의 시도**를 허용 | 음수 거부. 0은 폴백을 완전히 끈다. 어느 해석이든 방어 가능하고 차이가 요청 하나 전체이므로 명시한다 |
| `budget_ms` | int | `120000` | 첫 라우팅 호출부터의 라우팅 세션 전체 벽시계 상한 | 음수 거부 |

**`budget_exceeded`와 `auth`가 폴백할 수 없는 이유.** 초과된 예산에서 폴백하면 요청이 다른 배포로 가서
호출자가 요청한 적 없는 모델에 **다른 주체의** 예산을 쓴다. 그래서 예산 거부는 `429`가 아니라 종단
`400`이다 — `429`는 재시도를 유도하는 동시에 그 조건을 폴백 트리거로 표시하는 레이트 리밋 신호다. 인증
실패는 크리덴셜을 소진 표시하며, 다른 곳에서 재시도하면 같은 답에 도달하는 데 홉만 쓴다.

**스트리밍 경계.** 폴백은 첫 바이트가 클라이언트에 도달하기 전에만 허용된다. 그 이후에는 에러 이벤트가
스트림을 끝낸다. 중복 출력은 가시적 실패보다 나쁘고, 이 경계는 테스트로 강제된다.

> ⚠️ **opaque state는 대화를 프로토콜 계열에 고정하며 그 핀은 하드하다.** 대화가 계열 범위 opaque state를
> 싣는 순간 — 무결성 보호된 리즈닝 블록, 서버측 응답 핸들, 벤더 compaction 커서 — 계열을 가로지르는
> 폴백은 성공할 수 없다: 받는 계열이 외래 state를 절대 재시도 불가로 분류하므로, 남은 홉이 전부 타고
> 호출자는 같은 `400`을 세 배 느리게 받는다. [EXTENSIONS.ko.md](EXTENSIONS.ko.md) §B.3.

---

## 13. `pricing`

```yaml
pricing:
  catalog: /etc/dorang/pricing.yaml
  currency: USD
  rules:
    - id: plan-a-tokens
      class: marginal_usage
      match: {provider: plan-a, model: model-x}
      # 100만 토큰당 $2.50 / $10.00 / $0.25 — §13.1c.
      rates: {input: "2.50", output: "10.00", cached_read: "0.25"}
    - id: plan-a-subscription
      class: fixed_subscription
      match: {credential: plan-a-1}
      period: monthly
      amount: "20.00"
    - id: house-margin
      class: adjustment
      percent: "5"
```

**토큰 요율은 100만 토큰당이다.** 요율표를 옮겨 적기 전에 [§13.1c](#131c-요율은-무엇-하나당인가)를
먼저 읽을 것. 토큰당 값은 거부되지 않는다 — 모든 요청이 0원으로 가격 매겨질 뿐이다.
`dorangctl config lint`와 `dorang --check`가 그것을 경고하며, 그 외에는 아무것도 말해 주지 않는다.

### 13.1 키

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `catalog` | path | `""` | 외부 가격표, 인라인 규칙과 합쳐 하나의 카탈로그로 | **이름은 있는데 파일이 없는 것은 치명적이지 않다** — 인라인 규칙이 전체 가격표일 수 있고, 가격 없는 모델은 이미 카운터로 보인다. 형식이 잘못된 것은 치명적이다 |
| `currency` | string | `USD` | 모든 금액의 ISO 코드. 카탈로그 파일의 `currency`를 override | 빈 값 거부 |
| `rules[].id` | string | `""` | 규칙 정체성이자 최종 결정적 tie-break | 중복 id 거부 |
| `rules[].class` | `marginal_usage` \| `fixed_subscription` \| `adjustment` \| `notional_rate` | `marginal_usage` | 어떤 종류의 비용인가 | `notional_rate` 규칙은 `source`와 `as_of`를 실어야 하고, 다른 어떤 클래스도 그 둘을 실을 수 없다 (§8.5) |
| `rules[].priority` | int | `0` | id보다 먼저 specificity 동률을 가른다 | 음수 거부 |
| `rules[].match.{credential,provider,model,model_prefix,deployment}` | string | `""` | specificity 사다리 | 선언되지 않은 `provider`나 `credential`은 거부. `model`과 `model_prefix`는 선언된 모델과 대조되지 **않는다** — 현재 어떤 배포도 서빙하지 않는 모델을 정당하게 가격 매길 수 있다 |
| `rules[].rates.<component>` | decimal | — | 각 컴포넌트의 수량 **한 단위당** 요율이며, 토큰 5종의 그 한 단위는 **100만 토큰이다 — §13.1c**. 컴포넌트: `input`, `output`, `cached_read`, `cache_write`, `reasoning`, `request`, `characters`, `compute_seconds`, `audio_seconds`. **요율표는 배타적이다: 부분에 대한 요율은 그 부분을 부모에서 잘라낸다 — §13.1a.** 초당 요율이 어느 초를 가격 매기는지는 §13.1b | 요율 없는 `marginal_usage` 규칙 거부. `images`는 거부되며 대신 `request`를 지목한다: 요청 타입이 이미지 수를 싣지 않는다. `seconds`도 거부되며 그것을 대체한 두 축을 지목한다. **한 규칙이 서로 다른 단위의 컴포넌트를 섞을 수 없다** — `unit`은 규칙당 하나이므로 토큰과 초는 두 규칙으로 나눠 쓴다. **토큰당 값은 거부되지 않는다** — 모든 요청이 0원이 되는데도 `/spend/calculate`는 규칙이 매치됐다며 여전히 `"missing": false`를 답한다(§13.1c). 대신 *경고*된다: `config lint`와 `--check`가 100만당 `0.00001` 미만의 토큰 요율을 보고한다. 지수 표기(`1.25e-7`)와 소수점 아래 12자리 초과는 로더와 엔진 양쪽에서 **거부된다**(§13.1c) |
| `rules[].period` + `rules[].amount` | string + decimal | — | `fixed_subscription`의 주기와 비용 | 그 클래스에 둘 다 필수; 금액 0 거부 |
| `rules[].percent` | decimal | — | `adjustment`의 퍼센트 | 그 클래스에 필수; 0 거부 |

### 13.1a 요율이 어떤 관례로 적혀 있는가

**요율표는 배타(exclusive)이고 사용량 카운트는 포함(inclusive)이다. 부분에 대한 요율은 그 부분을
부모의 요율에서 잘라낸다.**

카탈로그 작성자가 알아야 할 것은 이 하나다. 벤더마다 관례가 다르고, 산술의 두 반쪽이 서로 다른
곳에서 오기 때문이다. dorang의 토큰 카운트는 **포함**이다(DESIGN §10.7): `input`은 캐시 읽기·쓰기
접두부를 안에 담은 프롬프트 전체이고, `output`은 추론 토큰을 안에 담은 응답 전체다. 벤더의 요율표는
그렇지 않다: `input`은 캐시에서 제공되지 *않은* 프롬프트 토큰의 단가이고, 캐시된 것에는 별도 단가가
있으며, 추론은 벤더가 따로 값을 매기지 않는 한 `output`에 접혀 있다.

그래서 적는 요율은 벤더의 것이고, dorang이 수량을 거기에 맞춘다:

| 요율 | 부과 대상 |
|---|---|
| `input` | `input_tokens − cache_read − cache_write` |
| `cache_read` | `cache_read` |
| `cache_write` | `cache_write` |
| `output` | `output_tokens − reasoning` |
| `reasoning` | `reasoning` |

각 뺄셈은 **그 규칙이 해당 하위 요율을 선언한 경우에만** 적용된다. 캐시에 대해 아무 말도 하지 않는
요율표는 캐시된 접두부를 `input` 요율로 청구한다 — 캐시 할인이 없는 벤더가 청구하는 방식 그대로다.
따라서 요율표 옮겨 적기는 기계적이다: 공표된 단가를 각자의 이름 아래 적고, 벤더가 따로 값을 매기지
않는 컴포넌트에는 아무것도 적지 않는다.

> ⚠️ **이 문단은 산술이 반대로 하고 있었기 때문에 존재한다.** `input`이 포함 프롬프트 전체에,
> `cache_read`가 그중 캐시된 부분에 *또* 부과되었다. §8.5의 예시 요율표(`input: 0.85`,
> `output: 3.40`, `cache_read: 0.19`)에서 40토큰 캐시 접두부를 가진 120토큰 프롬프트와 15토큰 응답은
> 벤더의 **$0.0001266 대비 $0.0001606 — 27% 초과**로 청구되었고, 캐시 읽기가 새 토큰의 1/10인
> 90% 캐시 에이전트 턴에서는 **5.7배**였다. §10.7은 "잘못 매핑된 캐시 필드는 보이는 오류가 아니라
> 잘못된 청구서를 만든다"고 이미 적고 있었다.
>
> 두 번의 깨끗한 패리티 실행과 한 번의 깨끗한 컷오버를 통과한 이유가 수정 자체보다 중요하다:
> **어느 하니스도 가격을 선언하지 않았다.** 매 실행이 0과 0을 비교했다. `testing/parity/`의
> 하니스 설정은 이제 가격을 매기고, 회귀 테스트는 dorang의 다른 함수가 아니라 손으로 계산한 벤더
> 금액에 대해 검사한다 — 내부 함수끼리의 비교는 관례 오류를 잡을 수 없다. 그것들은 서로 일치하고
> 있었다.

티어 구간은 잘라내기의 영향을 받지 않는다: 구간은 요청이 얼마나 큰가에 대한 진술이므로
`tiers[].up_to_input_tokens`는 포함 프롬프트 전체와 비교된다. `tier_mode: graduated`에서 캐시된
접두부는 구간 순회의 *앞쪽*에서 빠진다 — 캐시 히트는 문자 그대로 프롬프트의 접두부이기 때문이다.

사용량 보고가 자기모순이면 — 프롬프트 토큰보다 캐시 토큰이 많으면 — 부모의 부과액은 0까지만
내려가고 그 아래로는 가지 않는다. 음수 컴포넌트는 아무도 내지 않은 예산과 쿼터를 돌려주는 일이다.

### 13.1b 초당 요율은 어느 초를 가격 매기는가

**`seconds`는 없다.** 벽시계의 1초와 녹음된 오디오의 1초는 한 단어를 공유하는 서로 다른 두 청구 수량이고,
DESIGN §10.7의 규칙 — *청구 단위는 절대 변환되지 않는다* — 이 둘을 하나의 필드가 겸하는 대신 두 단위 위의
두 컴포넌트로 만든 이유다.

| 이렇게 적으면 | 이 단위에서 | dorang이 부과하는 것 | 쓰이는 곳 |
|---|---|---|---|
| `compute_seconds` | `per_compute_second` | **요청**이 걸린 시간 | self-hosted 배포의 GPU-초 / 점유 요율 |
| `audio_seconds` | `per_audio_second` | **벤더가 청구한 녹음**의 길이, `usage.seconds`에서 | 음성 인식, 그리고 매체 길이로 청구되는 그 밖의 모든 것 |

요율 이름이 축을 명명하므로 축을 말하지 않을 수 없다. `unit: per_second`와 맨 `seconds:` 요율은 두 대체
이름을 모두 지목하는 **로드 에러**다 — 그렇게 적는 오퍼레이터는 오타를 내는 것이 아니라 예전에 존재했던
유일한 철자를 적고 있는 것이다.

한 축으로 인용된 규칙은 다른 축만 실은 요청에 대해 다른 숫자로 **떨어지지 않는다**. 그 요청은 §8.3의
**가격 없음(UNPRICED)**으로 기록되고, 거절한 규칙이 로그와 `dorangctl price`에 이름으로 남으며, 아무것도
부과되지 않는다. 한 층 위, *벤더*가 `usage.type`에 진술한 단위에도 같은 것이 적용된다: 토큰으로 청구된
전사에 대한 `audio_seconds` 규칙도, 길이로 청구된 것에 대한 토큰 규칙도 둘 다 가격 없음이다. 두 경우 모두
산술은 성공해서 그럴듯한 수치를 내놓을 것이기 때문이다.

> ⚠️ **이 절의 두 번째 관례 결함이고 첫 번째보다 비쌌다.** `unit: per_second`가 요청의 경과 시간으로
> 채워지고 있었다. 전사 벤더는 녹음에 청구하므로 — **8초 만에 전사된 10분짜리 녹음이 8초로 청구됐다.**
> $0.006/분 기준 벤더의 $0.06에 대해 $0.0008, 즉 **청구서의 1.3%**를, 아무도 이의를 제기하지 않을
> 방향으로. 녹음 길이는 내내 옳게 디코드되고 있었다. 그것은 중립 transcript까지 도달해 거기서 멈췄다.
> 도착할 두 번째 축이 없었기 때문이다.
>
> 마이그레이션: 기존 게이트웨이의 `input_cost_per_second`나 `output_cost_per_second`에서 가져온
> 요율은 `compute_seconds`가 된다. 그것이 기존 게이트웨이가 곱하는 값 — 응답 시간 — 이기 때문이다.
> **그 모델이 오디오 길이로 청구한다면 `audio_seconds`로 바꿀 것**. 임포터는 그런 키마다 경고를 내지
> 대신 골라 주지 않는다. `input_cost_per_audio_per_second`는 그런 결정이 필요 없고 곧바로
> `audio_seconds`로 들어간다. 초당 키가 **둘 다** 실린 파일은 기존 게이트웨이가 유일하게 더하는
> 경우이며 — 둘 다 같은 경과 시간에 곱한다 — dorang에는 벽시계 요율이 하나뿐이다: import은 첫 번째를
> 남기고, 합을 추측하기를 거부하며, 그 사실을 말한다.

### 13.1c 요율은 무엇 하나당인가

**토큰 요율은 100만 토큰당이다.** `input: "2.50"`은 입력 100만 토큰당 2달러 50센트라는 뜻이고,
그것이 모든 벤더가 요율표를 인쇄하는 방식이다. 토큰 하나당 $2.50이 아니며, 같은 가격의 토큰당
표기인 `"0.0000025"`는 그 가격이 아니라 그 가격을 100만으로 나눈 값이다.

엔진은 요율에 수량을 곱하고 컴포넌트의 제수로 나눈다. 제수 표는 하나뿐이고 설정 불가다:

| 컴포넌트 | 단위 | 요율 하나가 덮는 양 | 제수 |
|---|---|---|---|
| `input`, `output`, `cached_read`, `cache_write`, `reasoning` | `per_1m_tokens` | **1,000,000 토큰** | 10⁶ |
| `request` | `per_request` | 요청 하나 | 1 |
| `characters` | `per_1k_characters` | 1,000자 | 10³ |
| `compute_seconds` | `per_compute_second` | 요청 자체의 벽시계 1초 (§13.1b) | 1 |
| `audio_seconds` | `per_audio_second` | 녹음된 오디오 1초 (§13.1b) | 1 |

이 절 맨 위의 규칙으로 계산해 보면 — 프롬프트 12,000 토큰 중 2,000이 캐시에서, 완성 800 토큰.
§13.1a의 잘라내기로 `input`에는 10,000이 청구된다:

```
10,000 / 1,000,000 × 2.50  = 0.025
 2,000 / 1,000,000 × 0.25  = 0.0005
   800 / 1,000,000 × 10.00 = 0.008
                             ------
                             0.0335 USD   (33,500,000 nanoUSD)
```

같은 요율표의 토큰당 표기로 같은 요청을 계산하면 **34 nanoUSD**, 곧 0.000000034 USD로 참값의 백만분의
일이다. 200 토큰쯤 아래로는 **정확히 0으로** 반올림되며, 대부분의 요청이 거기에 해당하고 원장 전체가
그렇게 보인다.

> ⚠️ **단위는 어디에도 적혀 있지 않았고, 배포되는 예제는 틀린 단위로 실려 나갔다.** 이 문서의 §13
> 예제와 `deploy/config.example.yaml`이 모두 토큰당 값인 `input: "0.0000025"`를 엔진이 100만당으로
> 읽는 필드에 넣고 있었다. 운영자가 확인할 수 있는 모든 것이 설정이 옳다고 말한다: 파싱되고,
> `dorangctl config lint`는 `ok`, `dorang --check`도 `ok`, 규칙은 선택되며,
> `POST /spend/calculate`는 **`"missing": false`**를 답한다 — `missing`은 매치된 규칙이 없다는
> 뜻인데 규칙은 매치됐기 때문이다. 유일한 증상은 0으로 가득 찬 원장이고, 그것은 요율이 틀렸다기보다
> 게이트웨이가 미터링을 안 하는 것처럼 읽힌다.
>
> `docs/DESIGN.md` §8.5의 카탈로그 예제는 처음부터 옳았으므로(`unit: per_1m_tokens` 아래 `"0.85"`),
> 배포되는 두 문서가 서로 어긋나 있었고 운영자가 복사하는 쪽이 틀린 쪽이었다. 예제는 이제 100만당이며,
> 테스트가 배포 파일로 알려진 요청을 가격 매겨 손으로 계산한 값과 대조한다 — 파일이 파싱되는지만
> 확인하는 테스트는 그 내내 통과했다.

**`pricing.rules`에는 `unit:` 키가 없다.** 인라인 형식은 사용한 컴포넌트 이름에서 단위를 유도하며,
그래서 한 규칙이 두 단위의 컴포넌트를 섞을 수 없다. 외부 카탈로그 파일에는 `unit:`이 *있고*,
**거기서 `unit:`이 없으면 `per_1m_tokens`이다** — 토큰당 숫자를 쓰고 `unit:`을 생략한 카탈로그 규칙은
똑같이 실패한다.

**incumbent에서 import하면 배율이 조정된다.** `dorangctl import config`는 실려 있는 모든 요율을
dorang이 가격 매기는 수량으로 변환하고, 몇 개를 변환했는지 한 번 보고한다. 변환은 정확하다 —
소수점이 움직일 뿐 반올림도 없고 어떤 값도 부동소수점을 거치지 않는다 — 그리고 명백한 두 필드가
아니라 표 전체다:

| incumbent가 쓰는 키 | dorang이 쓰는 것 | 배율 |
|---|---|---|
| `input_cost_per_token`, `output_cost_per_token`, `cache_read_input_token_cost`, `cache_creation_input_token_cost`, `output_cost_per_reasoning_token` | `input`, `output`, `cached_read`, `cache_write`, `reasoning` | **× 1,000,000** |
| `input_cost_per_character`, `output_cost_per_character` | `characters` | **× 1,000** |
| `input_cost_per_second`, `output_cost_per_second` | `compute_seconds` (§13.1b) | × 1 |
| `input_cost_per_audio_per_second` | `audio_seconds` (§13.1b) | × 1 |
| `input_cost_per_request` | `request` | × 1 |

incumbent가 표현할 수 있고 dorang이 표현할 수 없는 요율 — 이미지당, 픽셀당, *비디오* 초당, 별도의
오디오 토큰 요율, 배치 요율표, 128k 초과 티어 — 은 **import되지 않으며**, "이 파라미터는 대응물이
없다"가 아니라 하나씩 이름으로 보고된다. 돈은 실재하고, 다만 여기에 그 축이 없을 뿐이다.

importer는 지수 표기(`2.5e-06`)로 적힌 요율도 읽는다. 부동소수점을 출력하는 프로그램이 만든 파일에
들어 있는 것이 그 형태이기 때문이며, 읽어서 자릿수로 풀어 쓴다. 그 표기가 허용되는 곳은 여기뿐이다.

**요율은 자릿수로 적는다.** `2.5e-6`이 아니라 `0.0000025`. 지수 표기는 메인 파일과 카탈로그 파일
양쪽 모두에서 **로드 오류**다. 예전에는 둘 중 한쪽에서만 로드 오류였다 — `dorangctl config lint`는
`ok`라 답하고 게이트웨이가 그다음에 "exponent notation is not accepted"로 조립을 거부했으니, 배포
승인이 끝난 뒤에 도착한 로드 오류다. 소수점 아래 12자리를 넘는 값도 이제 마찬가지다. 엔진은 그것을
조용히 잘라내지 않는다.

**확인 방법.** 알려진 토큰 수로 `POST /spend/calculate`를 부르거나 `dorangctl price`를 써서 요율표와
손으로 대조한다. `"missing": false`는 그 확인이 아니다 — 규칙이 매치됐다는 뜻이지 그 규칙이 옳다는
뜻이 아니다.

`dorangctl config lint`와 `dorang --check`가 그중 한 부분은 대신해 준다: 100만당 `0.00001` 미만의
토큰 요율은 시장에서 가장 싼 요율표보다도 세 자릿수 아래이므로, 규칙과 요율을 지목하는 **경고**를
출력한다. 그것은 경고로 남고 결코 거절이 되지 않는다 — 100만 토큰당 $0.02인 정말로 싼 모델이
존재하고, 틀린 가격이 게이트웨이를 서비스 못 하게 만드는 원인이 되어서는 안 된다.

### 13.2 클래스는 경쟁하지 않고 합성한다

단일 승자 규칙 모델은 깨져 있다: 크리덴셜 범위 구독 규칙이 모델 범위 토큰 규칙을 이기고 토큰 비용을 0으로
만들며, 반대로 더하는 것은 "가장 구체적인 것이 이긴다"와 모순된다. 둘은 하나에 대한 경쟁하는 서술이 아니라
서로 다른 *종류*의 비용이다.

```
cost = marginal(승자) + amortized(구독 승자), 그다음 adjustment를 순서대로 적용
```

각 클래스가 자기 승자를 뽑는다. marginal 비용은 구독 규칙이 있든 없든 비트 단위로 동일하고, **라우팅은
marginal 값만 본다** — 매몰된 플랜 비용이 포화된 플랜을 싸 보이게 해서는 안 된다.

산술은 이진 부동소수를 절대 건드리지 않는다: 가격은 문자열 형태에서 파싱되고, 곱은 192비트 `mulDiv`를
거치며, 금액은 정산 버킷별로 나머지를 이월하며 half-to-even으로 한 번만 반올림된다. $0.0005/1M의 단일
토큰 요청 2000건이 전부 0으로 반올림되는 대신 정확히 1000 nano로 합산된다.

**추정과 정산은 다른 연산이다.** 이월 나머지와 주기 누적기는 *상태*이고, 비용 기반 라우터는 요청마다 모든
후보를 가격 매긴다 — 그래서 가격 계산에 부수효과가 있으면 진 후보들이 원장을 오염시킨다. 추정은 순수하고,
정산만 이월한다.

가격 없는 모델은 **회계상** 0이고 카운터를 증가시키며 경고를 남긴다. **라우팅**에서 가격 없는 모델은
*의견 없음*이지 가장 싼 것이 아니다. 0이 가장 싼 값이라, 아무도 가격 매기지 않은 배포가 영구히 조용히 모든
그룹을 이기게 되기 때문이다.

### 13.3 카탈로그 파일은 더 크고 다른 스키마다

외부 카탈로그는 `pricing.rules`와 같은 형태가 아니다. 인라인 형식에 없는 것을 지원하며, 컴파일 전에 하나의
문서로 합쳐진다:

| 인라인 `pricing.rules` | 카탈로그 파일 |
|---|---|
| `rates: {input: …, cached_read: …}` | `input:`, `cache_read:`가 **최상위** 규칙 필드 — `cached_read` → **`cache_read`** |
| `amount` + `period` | `amount_per_period` + `period` |
| `percent` | `op: percent` + `amount`, 그리고 `applies_to`, `order` |
| — | `unit:`, `tiers[]` + `tier_mode`, `when: {time_of_day, tz, weekday, date_range}` |
| — | **`class: notional_rate`**, `source:`와 `as_of:` 필수 |
| — | 파일 수준 `tz:` |

이름 변환은 한쪽 파일을 다른 쪽에 맞춰 바꾸는 대신 양방향 모두 의도적이다.

**`notional_rate`는 구독이 실제로 얼마짜리인가이다.** 정액 플랜은 고정 금액을 청구하므로 요청당 marginal
비용이 0이다 — 청구에는 옳고, 갱신할 가치가 있는지·어느 팀이 그 가치를 소비하는지·플랜을 넘어선 뒤 청구서가
어떻게 될지에는 무용하다. `notional_rate` 규칙은 같은 트래픽이 프로바이더의 종량 정가로 얼마였을지를
기록한다. 그것은:

- **구조적으로 청구에서 제외된다** — 별도 필드, 별도 롤업 컬럼, 총액에서 부재, 그리고 컴포넌트 내역에서도
  부재라 총액 대신 컴포넌트를 합산하는 호출자도 청구 값을 얻는다;
- **`source`와 `as_of`를 필수로 요구**하며, 경고가 아니라 로드 에러다. 출처 없는 요율은 통화 기호를 쓴
  추측이다. 그 두 필드를 *다른* 클래스에 쓰는 것도 거부된다. 조용히 무시되는 출처 필드는 없는 것과 같은
  결함이기 때문이다;
- **0이 아니라 없음으로 보고되며**, 가격 없음 플래그와 별도 플래그를 쓴다. 0은 구독을 무한히 효율적으로
  보이게 만들며, 그것은 가장 듣기 좋고 가장 의심받지 않을 답이다.

> 레버리지 비율을 믿기 전 주의 하나. 플랜 비용은 자기 기간에 걸쳐 발생하므로(§8.1) 분모가 사용량이 아니라
> *시간*으로 안분된다. 요청은 직전 요청 이후 발생한 몫을 기록하고, 한 기간의 몫의 합은 플랜 비용과 같으며
> 그보다 커지지 않는다. 전체 주기에 대해서는 비율이 맞지만, 주기 안에서는 한산한 시간이 낮은 레버리지로,
> 바쁜 시간이 훌륭한 레버리지로 읽힌다 — 둘 다 플랜에 대한 사실이 아닌데도. 사용량으로 가중되는 쪽은
> notional이며, 키별·팀별로 읽어야 할 값도 그쪽이다.

`dorangctl price <model> --input N --output N`으로 미리 볼 수 있으며, 원장이 돌리는 같은 평가기를 돌린다.

---

## 14. `metering`

```yaml
metering:
  numeric: {enabled: true}
  trace:   {store_messages: truncated, truncate_chars: 512, sample_rate: 1.0, daily_byte_budget: 8GiB}
  spool:   {dir: ~/.dorang/spool, max_bytes: 2GiB}
  flush_interval: 250ms
```

큐 둘. 수치 회계는 **절대 드롭되지 않는** CPU별 고정 카디널리티 카운터로 가고, 트레이스 페이로드는 상한 있는
링을 지나 내구 로컬 spool로 가며 **드롭 가능**하고 드롭은 계수된다.

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `numeric.enabled` | bool | `true` | 비용·토큰·에러 수 | ⚠️ **`false`는 거부**되며, 대신 `trace.sample_rate`를 낮추라는 메시지가 나온다. 수치 회계는 어떤 back-pressure에서도 살아남아야 하고, 두 큐 분리의 존재 이유가 그것이다 |
| `trace.store_messages` | `none` \| `hash` \| `truncated` | `truncated` | 메시지 본문 중 무엇이 `request_traces`에 도달하는가 | 그 외 거부. 요청을 다시 읽을 수 있게 해 주는 것은 `truncated`뿐이다 |
| `trace.truncate_chars` | int | `512` | 발췌 길이 | 음수 거부. 발췌는 substring으로 유지되는 대신 바이트 arena에 복사된다 — 400 KiB 본문의 512바이트 substring은 큐에 있는 동안 본문 전체를 붙든다 |
| `trace.sample_rate` | float | `1.0` | 트레이스 페이로드를 보관할 요청 비율 | `[0,1]` 이내. 엔터프라이즈 티어에서 100% 발췌는 하루 ~88 GB, 노트북 티어에서는 ~44 MB |
| `trace.daily_byte_budget` | size | `8GiB` | 트레이스 바이트의 하드 일일 상한 | 음수 거부. 희망이 아니라 강제된다 |
| `spool.dir` | path | `~/.dorang/spool` | 트레이스 큐와 스토어 사이의 내구 버퍼. 스토어 정체가 **데이터가 아니라 디스크**를 쓰게 한다 | 빈 값 거부. 빈 *값*과 쓸 수 없는 디렉터리는 다르다. 후자는 로드가 아니라 degraded 상태로 드러난다 |
| `spool.max_bytes` | size | `2GiB` | spool 자체 상한 | 음수 거부. 도달하면 트레이스를 드롭하고 계수한다 — 프로세스가 죽을 때까지 자라는 큐는 스토어 장애를 서비스 장애로 바꾼 것이다 |
| `flush_interval` | duration | `250ms` | 카운터 병합과 spool 출하 주기 | 0 이하 거부. 크래시가 정밀도를 잃는 창이기도 하다: 쿼터 링과 롤업은 마지막 upsert + 원장으로 재구성 가능하므로 꼬리를 잃는 것은 정확성이 아니라 정밀도의 비용이고, 이 간격이 그 양이다 |

**측정된 계측 비용:** 정상 상태 +148 ns, **버퍼 가득에서 +110 ns**, 즉 측정된 375 µs warm-local p50
예산의 0.039%(DESIGN §15.1 — 200 µs로 공표했으나 `testing/perf`가 재기 전까지 측정된 적이 없었고, 이후 두 번 정정되었다). 버퍼 가득이 *더 싼* 것은 분리가 설계대로 동작하는 것이다 — 실패한 링 push는 페이로드 복사를 건너뛰고
수치 경로는 동일한 일을 한다. "계측 on/off < 5%"에는 분모가 필요했다: no-op meter 대비 비율은 7.6×지만
no-op은 분기 하나 뒤에 반환하므로 그것으로 나누는 것은 실제 요청에 대해 아무것도 재지 않는다. 요구사항은
게이트웨이 오버헤드 예산의 5%다.

샘플링 제외는 의도적으로 degraded 플래그를 올리지 **않는다.** 혼동하면 샘플링된 배포에서 플래그가 영구히
참이 되고, 그것은 플래그가 없는 것과 같다.

> **이 자리에는 "degraded 상태는 이 빌드에서 관측 불가능하다"가 적혀 있었고, 더 이상 참이 아니다.**
> `internal/metrics/collect_meter.go`가 `dorang_metering_degraded`와 `dorang_metering_degraded_reason`을
> 게시하고, `internal/app`의 health reporter가 그것을 `GET /health`로 잇는다 — 그 전까지 meter는 히스테리시스와
> 함께 다섯 가지 degradation 사유를 추적했고 그 결과를 읽는 것이 어디에도 없었다. §23.1a.

---

## 15. `observability`

```yaml
observability:
  prometheus: true
  otlp_endpoint: ""
  log_level: info
  log_format: json
  always_full_headers: false
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `prometheus` | bool | `true` | `GET /metrics`를 서빙한다 | `false`는 라우트를 없애고, 그러면 다른 미서빙 라우트와 마찬가지로 이유를 붙여 501을 답한다 — 빈 본문의 200이 아니다 |
| `metrics.public` | bool | `false` | 인증 없이 `/metrics`를 서빙한다 | 기본은 꺼짐. 스크레이프는 키별 지출, 크리덴셜별 쿼터 상태, 설정된 모든 모델 이름을 싣기 때문에, 배포가 명시적으로 달리 말하지 않는 한 master 크리덴셜을 요구한다 |
| `otlp_endpoint` | string | `""` | OTLP 트레이스 export 대상 | ⚠️ 이 빌드에서 작동하지 않는다; 지연 내역은 어쨌든 기록된다. §23.1 |
| `log_level` | `debug` \| `info` \| `warn` \| `error` | `info` | 로그 상세도 | 그 외 거부 |
| `log_format` | `json` \| `text` | `json` | 로그 인코딩 | 그 외 거부 |
| `always_full_headers` | bool | `false` | 호출자가 `x-dorang-detail: full`을 보낼 때만이 아니라 **모든** 응답에 전체 확장 헤더 집합을 붙인다 | 켜면 첫 스트리밍 바이트보다 앞에 약 30개 헤더가 추가되어 중간 장비의 헤더 크기 제한 위험이 있고, 그것이 서빙하려던 지연 목표에 비용을 물린다 |

**뒤집기 쉬운 헤더 규칙:** 클라이언트가 **행동하는** 헤더는 무조건, **읽는** 헤더는 게이팅 가능. 그래서
`Retry-After`와 `x-ratelimit-*` 집합은 항상 붙는다 — `Retry-After`를 텔레메트리 플래그 뒤에 두면 모든
SDK의 백오프가 `429`에서 조용히 동작을 멈춘다. dorang 자신의 헤더 중에는 `x-dorang-request-id`, `-model`,
`-upstream-model`, `-deployment`, `-cost-usd`, 그리고 false일 때의 `-replayable`만 무조건이다. 전체 목록은
[OPERATIONS.ko.md](OPERATIONS.ko.md) §6.

---

## 16. `extensions`

```yaml
extensions:
  lua:
    enabled: false
    dir: /etc/dorang/lua
    hooks: [on_request, on_route, on_response, on_email]
    limits: {instructions: 5000000, memory_mb: 32, timeout: 200ms}
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `lua.enabled` | bool | `false` | 샌드박스 훅 활성화. 꺼져 있으면 엔진이 typed nil이고 핫패스는 nil 검사 하나 비용이다 | — |
| `lua.dir` | path | `/etc/dorang/lua` | 훅 프로그램 위치 | 활성화 시 빈 값 거부 |
| `lua.hooks[]` | []string | `[]` | 허용: `on_request`, `on_route`, `on_response`, `on_email` | 그 외 거부 |
| `lua.limits.{instructions,memory_mb,timeout}` | int/int/duration | `5000000` / `32` / `200ms` | 훅별 상한 | 음수 거부. **활성화된 훅에서 셋 중 하나라도 0이면 거부** — 핫패스의 무제한 훅은 훅이 아니라 나쁜 프로그램을 기다리는 장애다 |

의미론: 상한 초과는 훅을 건너뛰고 경고한다(**fail-open**). 예외는 `on_request`의 명시적 거부이며 그것은
존중되고(**fail-closed**), 또 하나의 예외는 §10.5b의 마스킹 필터로 아예 돌 수 없을 때 거부한다. 훅 안의
panic은 격리된다. 어떤 훅도 비밀값을 볼 수 없다 — 훅에 건네지는 view는 나중에 걸러내는 것이 아니라 애초에
키 자료 없이 구성된다.

자기 deadline을 반복해서 무시하는 훅은 프로세스 수명 동안 꺼진다. 그리고 그것은 fail-*open*인 네 훅에만
적용된다: 마스킹 필터를 끈다고 거부가 멈추지는 않고 거부가 영구화될 뿐이기 때문이다. 어느 쪽이든
goroutine을 제한하는 것은 훅당 고정된 공급량이다 — 동시에 최대 32개가 돌고 그중 최대 8개가 버려질 수
있으며, 그것을 넘으면 goroutine이 생기기 전에 호출이 거부된다.

**확장을 쓰는 세 가지 방법, 하나의 계약.** `extensions.lua.dir`은 `*.policy` 파일을 담는다 — 루프도
호출도 문자열 생성도 없는 total 정책 언어라 프로그램이 구성상 종료한다. Lua **플러그인**은 여기가
아니라 [`filters.plugins`](#17-filters)에서 선언한다. §11.5가 로딩을 명시적 설정으로 요구하기
때문이다 — 안에 나타나는 것을 무엇이든 실행하는 디렉터리는 코드 실행 프리미티브다. 컴파일된 Go
`Native` 훅이 세 번째다. 셋 다 같은 비밀값 없는 view를 보고 같은 규칙을 따른다(`Native`에는 벽시계와
panic 격리만 적용된다 — Go 코드이므로 메모리 상한은 의미가 없다).

**상한에 대하여.** VM은 gopher-lua, 순수 Go, 바이너리 약 1.8 MB다. 벽시계는 VM이 제공하고(명령어
사이에 context 검사가 돈다) 나머지 둘은 제공하지 않는다: state별 명령어 카운터도 state별 할당 회계도
없다. 둘 다 철회하지 않고 그 위에 다시 구현했다.

- *명령어*는 로드 시점에 플러그인의 구문 트리를 재작성해 과금한다: 모든 함수 본문, 모든 루프 본문, 모든
  후방 `goto`에 과금 — *Lua 소스*가 무한히 실행될 수 있는 방법은 그 셋뿐이다. 그래서 `instructions`는
  단순한 back-edge가 아니라 작업량을 센다 — 그러나 소스 재작성은 builtin 안을 볼 수 없으므로, 반환값으로
  작업량이 제한되지 않는 builtin은 돌기 *전에* 그 작업량을 과금한다. 패턴 계열(`find`, `match`, `gmatch`,
  `gsub`)은 호출자가 공급하는 subject에서 초선형으로 백트래킹하고, `tonumber`는 숫자 하나를 반환하려고
  인자 전체를 읽는다. 둘 다 예산이 붙은 matcher 복사본에 먼저 돌려 값을 매긴다. *연산자* 셋도 같은 문제를
  갖고 과금할 호출이 없다 — `a == b`, `a < b`, `t[k]`는 호출자가 크기를 정한 문자열의 모든 바이트를 걷거나
  해싱한다 — 그래서 재작성은 비교 피연산자와 동적 테이블 키를 그 길이에 대한 과금으로 감싸고 연산자
  자체는 VM에 남긴다. 양쪽 비용이 모두 로드 시점에 고정되지 *않은* 자리에만 과금하므로 리터럴과의 비교,
  `t.field`, `t[i]`, `i <= n`은 비용이 0이다. `rawequal`, `rawget`, `rawset`, `next`, `pairs`, 비교자 없는
  `table.sort`도 맞춰 값이 매겨진다. 측정치와 남은 하나의 유계 잔여 위험(플러그인이 만든 `__index` 체인)은
  DESIGN §11.5.
- *스택*은 따로 제한된다. 작업량 과금은 CPU를 제한하지 재귀적 호스트 호출이 키우는 goroutine 스택을
  제한하지 않기 때문이다. 패턴 matcher는 Go 프레임 하나당 한 갈래를 탐색하므로 greedy 수량자는 소비하는
  문자마다 프레임 하나를 쓴다: 호출자 메시지 텍스트 800 KiB에 대한 `^(.*)=(.*)$`는 동시 요청 8개에서 어떤
  상한도 발화하지 않은 채 3 GB를 썼다. 재귀는 10 000 프레임 — 매치당 약 4 MB — 으로 제한되고, 그 귀결은
  만나기 전에 알아 둘 값어치가 있다: **수량자가 한 번에 10 000자 넘게 실어야 하는 Lua 패턴은 거부된다.**
  그러니 문서 전체를 훑는 필터는 Lua에서 백트래킹하는 대신 `dorang.mask`(Go regexp, 재귀 없음)를 쓸 것.
  값이 매겨진 매치는 호출의 context도 함께 받으므로 `timeout`도 그것을 제한한다.
- *메모리*는 할당을 과금한다. 모든 할당은 과금당 O(1)이거나 — 그래서 명령어 상한으로 제한된다 — 일어나기
  전에 과금된다: 문자열 연결은 과금되는 호스트 호출로 재작성되고, `string.rep`, `string.format`,
  `string.gsub`, `string.byte`, `string.char`, `unpack`, `table.concat`은 최악의 경우를 미리 계산한다.
  예산은 **실시간이 아니라 누적**이다: 과금된 바이트는 절대 환불되지 않는다. 호스트는 Lua의 수집기가
  문자열을 언제 해제하는지 볼 수 없기 때문이다. 그것이 최대 메모리를 위에서 제한하고, 대신 할당과 해제를
  반복하는 장수 훅은 실제 사용량이 마땅한 것보다 일찍 상한에 닿는다.

**샌드박스가 담지 않는 것**, 각각 이유가 있다: `io`, `os`, `debug`, `coroutine`과 패키지 로더는 아예 열리지
않는다. `load`, `loadstring`, `loadfile`, `dofile`, `require`는 각각 계측기를 통과한 적 없는 텍스트를 함수로
바꾸므로 제거됐다. `_G`, `getfenv`, `setfenv`는 과금 함수가 사는 전역 테이블을 넘겨주므로. `pcall`과
`xpcall`은 플러그인이 잡을 수 있는 상한은 상한이 아니므로. `math.random`은 자기 입력의 순수 함수가 아닌
필터가 턴마다 다른 업스트림 바이트를 만들어 prefix 캐싱을 조용히 죽이므로.

> ⚠️ **이 자리에는 "절 이름이 `lua`인데 Lua 인터프리터가 없다"가 적혀 있었고, `a0d5871` 이후로 거짓이다.**
> `go.mod`가 `github.com/yuin/gopher-lua v1.1.2`를 요구하고, `internal/luaext`가 샌드박스 VM이며,
> `internal/app/extensions.go`와 `internal/app/filter.go`가 요청 경로에서 그것을 만들고 컴파일한다.
> `94de856`·`bb67140`·`241f73b`가 굳혔다. 영문 §16은 이미 고쳐져 있었고 이 미러만 남아 있었다 — 그리고
> 옳게 살아남은 좁은 주장은 아래의 로드 에러다: 거부의 대상은 언어가 아니라 *디렉터리를 스캔하는 것*이다.

> **`extensions.lua.dir` 아래의 `.lua` 파일은 조용히 무시되는 파일이 아니라 로드 에러다**, 그리고
> 메시지가 그 파일이 있어야 할 자리를 말해 준다. 돌리지도 않고 — 그러면 디렉터리가 코드 실행
> 프리미티브가 된다 — 조용히 무시하지도 않는다. 후자는 운영자가 강제되고 있다고 믿는 필터를 남긴다.

> 훅 지점이 의도적으로 갖지 **않는** 능력 하나: `on_route`는 선택된 배포를 보고 거부할 수 있지만 다른 것을
> 요구할 수는 없다. 그것은 훅이 아니라 fail-back 기계의 성질이다.

---

## 17. `filters`

변환 필터는 canonical 요청과 백엔드 사이에 앉는다(§10.5b). 동기가 된 사례는 되돌릴 수 있는 PII 마스킹이다:
주민등록번호가 오퍼레이터의 통제를 떠나기 전에 자리표시자로 바뀌고 응답에서 복원된다.

```yaml
filters:
  secret:
    key_env: DORANG_FILTER_SECRET
  plugins:
    - name: pii-mask
      path: /etc/dorang/plugins/pii_mask.lua
      fail: closed
      config: {skip_system: "true"}

models:
  - name: model-x
    filters:
      - plugin: pii-mask
        on: [request, response]
        scope: conversation
        patterns: [krrn, email, {name: employee_id, regexp: 'EMP-\d{6}'}]
        retain: 1h
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `secret` | secret ref | — | 자리표시자를 파생시키는 클러스터 전역 시드 | **모든 필터가 `scope: request`를 쓰지 않는 한 필수.** 모든 노드에서, 그리고 재시작을 가로질러 같아야 한다: 프로세스별 시드는 같은 텍스트를 어디서나 다르게 마스킹하므로 백엔드에 도달하는 본문이 바이트 단위로 달라지고 모든 prefix 캐시가 콜드 스타트한다(§7.4b). 이것을 교체하면 fleet의 모든 캐시가 무효화된다 — 단계적으로, 공지하고 할 작업이다 |
| `plugins[].name` | string | — | 모델이 이 플러그인을 부르는 이름 | 유일해야 한다. 선언되지 않은 플러그인을 지목하는 모델은 로드에서 거부 |
| `plugins[].path` | path | — | Lua 파일. 명시적으로 이름 지어지며 절대 스캔되지 않는다 | 없는 파일은 조용히 아무것도 하지 않는 필터가 아니라 기동 실패다 |
| `plugins[].fail` | `closed` \| `open` | `closed` | 플러그인이 완료하지 못하거나 **아예 돌지 않았을 때** 무엇이 일어나는가 | `closed`는 요청을 거부한다. 이것은 §11.5의 fail-open을 의도적으로 뒤집는다: *풍부하게* 만들지 못한 필터는 건너뛰는 게 맞지만, 주민번호를 지우기로 되어 있었는데 지우지 않은 필터는 요청을 멈춰야 한다. "돌지 않았다"도 포함이다: `on_filter_request` 핸들러를 등록하지 않은 플러그인이나 꺼진 훅은 중간에 실패한 플러그인과 똑같이 거부한다 — 대안은 설정된 마스킹 필터가 텍스트를 조용히 업스트림으로 보내는 것이다 |
| `plugins[].config` | map[string]string | `{}` | 로드되는 동안 `dorang.config`로 플러그인에 건네진다 | 비밀값을 담지 않는다. 신뢰할 수 없는 코드가 읽을 수 있다 |
| `models[].filters[].plugin` | string | — | 어느 선언된 플러그인이 도는가 | 매달린 이름은 거부 |
| `models[].filters[].on` | []string | `[request, response]` | 어느 반쪽이 도는가 | `[response]` 단독은 거부: 마스킹된 것이 없으면 되돌릴 것도 없다 |
| `models[].filters[].scope` | `conversation` \| `principal` \| `tenant` \| `request` | `conversation` | 자리표시자의 안정성이 어디까지 닿는가 | 아래 참조 — 프라이버시와 성능의 교환이 들어 있는 유일한 노브다 |
| `models[].filters[].patterns[]` | 이름 또는 `{name, regexp}` | — | 무엇이 마스킹되는가. 내장: `krrn`, `email` | 빈 문자열에 매치될 수 있는 패턴은 거부. regexp 없는 미지의 이름도 거부 |
| `models[].filters[].retain` | duration | `1h` | 역방향 테이블이 매핑을 메모리에 붙들고 있는 시간 | 프로바이더가 자기 대화 상태를 유지하는 시간 이상이어야 한다. 메모리에만 있고 어디에도 기록되지 않는다 |

**scope가 교환의 전부이고, 그것은 피할 수 없다.** 자리표시자는 파생된다 —
`HMAC(secret, scope ‖ salt ‖ value)` — 그래서 같은 텍스트는 언제나 같은 바이트로 마스킹되고, 그것이
업스트림 prefix를 턴을 가로질러 동일하게 유지해 백엔드의 KV 캐시를 살려 둔다. 대가는 결정적 자리표시자가
안정적인 가명이라는 것이다: 그 scope 안에서 프로바이더는 "이 같은 사람이 이 요청들에 나타난다"를 이을 수
있다. 그것은 캐시 히트 옆의 버그가 아니라 캐시 히트와 같은 성질이다.

| `scope` | 안정 범위 | prefix 캐싱 | 연결 가능성 |
|---|---|---|---|
| `conversation` | 한 대화의 턴들 | 동작 | 프로바이더가 이미 보지 않은 것은 없다 |
| `principal` | 한 키가 보내는 모든 것 | 대화를 가로질러 동작 | 한 키의 트래픽이 이어진다 |
| `tenant` | 한 팀 | 팀을 가로질러 동작 | 한 팀의 트래픽이 이어진다 |
| `request` | 없음 | 그 경로에서 **꺼진다** | 없음 |

`request`는 절대 반복될 수 없는 바이트에 대한 주장을 남기는 대신 그 요청의 prefix 어피니티를 끈다.

**조용히 두지 않고 적어 두는 한계 셋.** thinking 블록의 텍스트는 마스킹되지 않는다(프로바이더의 무결성
자료와 함께 다니며 dorang이 바이트 단위로 그대로 재생한다). 툴 호출의 인자도 마스킹되지 않는다(중립
형식이 그것을 원본 JSON으로 유지하는 이유는 재인코딩이 키 순서를 바꾸기 때문이고, 그것을 재인코딩하는
필터는 위의 결정성을 깨뜨린다). 필터가 텍스트에 닿을 수 없는 표면 — 예를 들어 임베딩 — 은 필터가 붙은
모델에서 필터 없이 보내지는 대신 **거부된다**.

**카운터.** `dorang_filter_masked_total`, `_restored_total`, `_unresolved_total`, `_refused_total`.
`_unresolved_total`이 오르는 것은 모델이 이 게이트웨이가 발행한 적 없는 자리표시자 모양의 텍스트를 내고
있다는 뜻이고, 그것은 알 가치가 있다. 어떤 카운터도 호출자 텍스트에서 파생된 라벨을 싣지 않으며, 어떤
자리표시자나 원본도 로그 줄·메트릭·원장·트레이스에 도달하지 않는다.

---

## 18. `passthrough`

프로바이더 네이티브 라우트를, 매번 어댑터를 쓰는 대신 설정으로 연다.

```yaml
passthrough:
  enabled: false
  routes:
    - {prefix: /anthropic, provider: cloud-a}
    - {prefix: /plan-a, provider: plan-a, auth: dorang, meter: true}
  default: {auth: dorang, meter: true, timeout: 600s}
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `enabled` | bool | `false` | 엔진 활성화. **매핑되지 않은 프리픽스는 서빙되지 않는다: 오픈 프록시가 아니다** | 꺼져 있으면 패스스루 라우트가 아예 등록되지 않는다 |
| `routes[].prefix` | string | — | 매핑할 경로 프리픽스 | 빈 값, `/`로 시작하지 않음, `..` 포함, 중복 — 전부 거부. `..`을 거부하는 것은 결합 경로가 정규화되고 traversal이 거부되기 때문이며, 로드에서 거부하는 것이 정규화기가 옳음을 증명하는 것보다 싸다 |
| `routes[].provider` | string | — | 나머지 경로를 결합할 대상 | 선언돼 있어야 한다. ⚠️ **`base_url`이 없는 프로바이더의 라우트는 조용히 삭제된다** — 라우트가 아예 존재하지 않고 501로 알게 된다 |
| `routes[].auth` | `dorang` \| `client` \| `none` | `default.auth` | `dorang`은 dorang 키로 인증, `client`는 호출자 크리덴셜을 통과, `none`은 인증 없음 | 그 외 거부. `none`은 그 프리픽스를 프로바이더로 향하는 공개 프록시로 만든다 |
| `routes[].meter` | bool | `default.meter` | 최선 노력 계측 | 끄면 그 프리픽스를 통한 지출이 보이지 않는다 |
| `routes[].timeout` | duration | `default.timeout` | 라우트별 타임아웃 | 음수 거부 |
| `default.auth` / `.meter` / `.timeout` | — | `dorang` / `true` / `600s` | override하지 않는 라우트에 적용 | — |

**보안 규칙 넷, 뒤의 둘은 초기 초안에 없었다:**

1. 매핑되지 않은 프리픽스는 서빙되지 않는다.
2. 결합 경로는 정규화되고 traversal은 거부된다.
3. **리다이렉트는 절대 따라가지 않는다.** 업스트림의 `30x`는 평범한 HTTP 클라이언트가 요청을 —
   *프로바이더 크리덴셜을 붙인 채* — **업스트림이** 고른 호스트로 재발행하게 만든다. 그것은 손상되거나
   오설정된 백엔드를 dorang에 대한 접근 없이도 크리덴셜 유출 프리미티브로 바꾼다.
4. **크리덴셜은 양방향에서 제거된다.** "클라이언트에 절대 도달하지 않는다"가 요청 경로에 대해서만
   진술돼 있었다. 받은 키를 되돌려주는 백엔드가 있으면 그대로 중계됐을 것이다.

⚠️ **프리픽스 맵은 허용목록이 아니다.** 두 self-hosted 엔진 모두 파괴적인 개발 모드 라우트를 추론과 같은
base URL에 둔다 — vLLM의 `/reset_prefix_cache`는 실행 중 모든 요청을 선점할 수 있고, SGLang의
`/flush_cache`, `/slow_down`, `/pause_generation`은 키 없는 서버에서 기본 열림이다. `/vllm`을 매핑하면
그것들도 매핑된다. 이 빌드에는 라우트별 method+path 허용목록이 없다; 생기기 전까지는, 그 프리픽스에 닿을
수 있는 누구에게든 표면 전체를 노출할 각오가 된 프로바이더에만 프리픽스를 매핑할 것.
[EXTENSIONS.ko.md](EXTENSIONS.ko.md) §E3.1, [SGLANG.ko.md](SGLANG.ko.md) §8.4.

---

## 19. `shadow`

마이그레이션 중 라이브 트래픽의 표본을 참조 게이트웨이와 비교한다. 전체 절차는
[MIGRATION.ko.md](MIGRATION.ko.md) §4.

```yaml
shadow:
  mode: "off"
  reference: {url: "", api_key_env: DORANG_SHADOW_REFERENCE_KEY, timeout: 60s}
  sample_rate: 0.05
  compare: {structural: true, semantic: false, ignore_fields: []}
  max_cost_usd_per_day: "5"
  unpriced_estimate_usd: "0.01"
  queue_size: 256
  workers: 4
  capture: {head_bytes: 256KiB, tail_bytes: 16KiB}
  report: {path: ~/.dorang/shadow.jsonl, max_bytes: 256MiB}
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `mode` | `off` \| `mirror` \| `compare` | `off` | `mirror`는 보내고 기록, `compare`는 보내고 기록하고 diff | 그 외 거부. `"off"`를 따옴표로 감쌀 것 — 맨 `off`는 YAML boolean false다 |
| `reference.url` | string | `""` | 비교 대상 게이트웨이 | mode가 `off`가 아니면 필수. `http`/`https`여야 하고, host가 있어야 하며, **쿼리나 프래그먼트가 없어야 한다** — 둘 다 요청 경로와 결합되면 살아남지 못한다 |
| `reference.api_key_env` | string | `""` | **참조 게이트웨이 자신의** 크리덴셜 변수 이름. 클라이언트 크리덴셜은 절대 그리로 전달되지 않는다 | 이 파일에서 유일하게 `key_env`/`key_file` 짝이 아니라 `api_key_env`로 비밀 참조를 쓰는 곳이다. **비밀값을 파일이나 볼트에 두는 배포는 shadow 참조 크리덴셜을 아예 표현할 수 없다** |
| `reference.timeout` | duration | `60s` | 참조 호출 하나의 상한 | 음수 거부. `server.request_timeout`보다 의도적으로 짧다: 자기가 복사한 요청보다 오래 사는 shadow 호출은 아무도 읽지 않을 비교를 위해 워커를 붙들고 있는 것이다 |
| `sample_rate` | float | **`0`** | shadow할 적격 요청 비율 | `[0,1]` 이내. ⚠️ **기본값이 없다.** 이것을 설정하지 않고 `mode`를 켜면 아무것도 shadow하지 않고, 빈 리포트를 쓰고, 게이트 판정 `no_data`를 낸다 — 그리고 빈 리포트는 컷오버 결정이 찾는 바로 그것이다 |
| `compare.structural` | bool | `true` | 상태, 필드 경로 집합, 경로별 타입, 헤더 키, 에러 봉투를 비교 | `mode: compare` + structural off는 "대신 `mode: mirror`를 쓰라"며 거부 |
| `compare.semantic` | bool | `false` | — | ⚠️ **true는 거부된다.** 설계가 노브를 명명하고 비교를 명세하지 않는다. 조용히 받아들이면 의미 비교를 요청한 오퍼레이터가 구조 비교와 빈 리포트를 받고 그것을 검사한 적 없는 무언가의 증거로 읽는다 — 컷오버 게이트가 가질 수 없는 유일한 실패 모드다 |
| `compare.ignore_fields[]` | []string | `[]` | 내장 집합(id, 타임스탬프, `system_fingerprint`, 출력 텍스트, 토큰 수) 위에 추가로 제외할 필드 경로. 맨 이름은 임의 깊이 매칭, `$.` 시작은 정확 매칭, 후행 `*`는 프리픽스 매칭 | 빈 항목 거부. 무시하는 모든 필드는 clean 리포트가 아무 말도 하지 않는 필드다 |
| `max_cost_usd_per_day` | decimal | — | 참조 게이트웨이 지출의 일일 상한 | ⚠️ mode가 `off`가 아니면 **필수**. 두 모드 모두 표본 요청을 두 번 보내므로 두 배 비싸다. 이전 리비전은 `mirror`에만 상한을 요구했는데 그것은 거꾸로다 — `compare`는 `mirror` + diff다 |
| `unpriced_estimate_usd` | decimal | `0.01` | dorang이 복사 대상 요청의 가격을 매기지 못했을 때 shadow 호출 하나에 부과할 금액 | 의도적으로 0이 아니다. 0을 부과하면 비용을 모르는 트래픽 — 상한을 씌울 가치가 가장 큰 트래픽 — 에 대해 상한이 강제 불가능해진다 |
| `queue_size` | int | `256` | shadow 작업 큐 상한. 넘으면 드롭되고 계수된다 | 0 이하 거부. 가득 찬 큐가 요청 경로로 back-pressure를 밀어서는 절대 안 된다 |
| `workers` | int | `4` | 동시 참조 호출 수 | 0 이하 거부 |
| `capture.head_bytes` | size | `256KiB` | 응답 앞부분에서 보관할 바이트 | 0 이하 거부 — 비교에는 비교할 응답이 필요하다. 평범한 chat 응답이 통째로 잡힐 만큼 크며, 그것이 비교를 결론 가능하게 유지한다 |
| `capture.tail_bytes` | size | `16KiB` | 뒷부분에서 보관할 바이트 | 음수 거부. 창이 하나가 아니라 둘인 것은 스트림의 종료자가 와이어의 마지막이고 head 버퍼 하나로는 정확히 그것을 잃기 때문이다 |
| `report.path` | path | `~/.dorang/shadow.jsonl` | JSONL diff 리포트 | **mode가 `compare`면 필수**: 빈 diff 리포트가 완료 기준이고, 쓰이지 않은 리포트는 빈 것이 아니라 부재한 것이다 |
| `report.max_bytes` | size | `256MiB` | 리포트 상한 | 음수 거부. 도달하면 레코드를 드롭하고 계수한다 — 잘린 리포트를 빈 리포트로 읽는 것이 이 메커니즘 최악의 결과다 |

`queue_size × (head_bytes + tail_bytes)`가 shadow가 보유할 수 있는 메모리다: 기본값에서 최악 256 × 272 KiB
≈ 68 MiB.

---

## 20. `notifications`

이메일 시스템이 아니라 **싱크를 갈아 끼울 수 있는 이벤트 버스**다. `email` 아래는 전달이고, 그 옆의 모든
것은 파이프라인이다 — 파이프라인이 존재하는 이유는 알림이 지연 쓰기이고 §9.6이 지연 쓰기에 상한이 있고,
드롭될 때 보이고, 절대 요청 경로에 있지 않기를 요구하기 때문이다.

```yaml
notifications:
  email:
    driver: none                  # smtp | http | lua | none
    from: dorang@example.invalid
    to: [ops@example.invalid]
    smtp: {addr: "mail.internal:587", username: dorang, key_env: SMTP_PASSWORD,
           starttls: true, tls_skip_verify: false, timeout: 10s, helo: ""}
    http: {url: "https://hooks.example.invalid/dorang", key_env: DORANG_WEBHOOK_SECRET,
           timeout: 10s, headers: {}}
  events: [key_created, budget_80pct, budget_exceeded, quota_exhausted,
           credential_unhealthy, batch_completed, invite]
  queue_size: 256
  workers: 1
  dedup_period: 1h
  dedup_periods: {}
  retry: {max_attempts: 3, initial_backoff: 1s, max_backoff: 30s,
          breaker_threshold: 5, breaker_cooldown: 1m}
```

### 19.1 파이프라인

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `events[]` | []string | `[]` | 허용: `key_created`, `budget_80pct`, `budget_exceeded`, `quota_exhausted`, `credential_unhealthy`, `batch_completed`, `invite` | 그 외 거부 |
| `queue_size` | int | `256` | 요청 경로와 sender 사이 큐 상한. 도달하면 **드롭하고 계수한다** | 음수 거부. 알림당 수백 바이트이므로 상한 있는 메모리이고 건강한 배포가 보유하는 것보다 훨씬 크다. 대신 자라는 큐는 멈춘 메일 서버를 요청 경로 안에 집어넣는다 |
| `workers` | int | `1` | 전달 워커 | 음수 거부. **1은 의도적이다**: 메일은 처리량 문제가 아니고, 이미 실패 중인 서버를 워커 N개가 두들기는 것이 breaker가 막으려는 동작이다 |
| `dedup_period` | duration | `1h` | 한 subject가 같은 이벤트를 얼마나 자주 올릴 수 있는가 | 음수 거부. 없으면 `budget_80pct`가 **임계값을 넘은 모든 요청마다** 발화한다 — 오퍼레이터가 읽는 것과 필터링하는 것의 차이다. 한 시간은 예산이 아직 80%라는 것을 상기시키고 싶은 간격이지 요청이 도착하는 간격이 아니다 |
| `dedup_periods.<event>` | duration | — | 이벤트별 override. **0은 그 이벤트의 중복 제거를 끈다** | 알 수 없는 이벤트 이름이나 음수 거부. `key_created`는 구성상 한 번만 일어나며 중복 제거되지 않는다 |
| `retry.max_attempts` | int | `3` | 드롭하고 계수하기 전 전달 시도 수 | 음수 거부 |
| `retry.initial_backoff` | duration | `1s` | 첫 재시도 간격 | 음수 거부 |
| `retry.max_backoff` | duration | `30s` | 백오프 상한 | 음수 거부, **그리고 `initial_backoff`보다 짧으면 두 값을 명시하며 거부** |
| `retry.breaker_threshold` | int | `5` | 전달 시도 자체를 멈추기까지의 연속 실패 수 | 음수 거부 |
| `retry.breaker_cooldown` | duration | `1m` | breaker가 열려 있는 시간 | 음수 거부 |

요청 경로는 정확히 두 가지만 한다: 이 알림이 이미 회계됐는지 묻고(샤딩된 map 조회, 무할당), 아니면
알림을 넘긴다(논블로킹 enqueue). 렌더링·redaction·연결·재시도는 전부 워커에서 일어난다.

### 19.2 드라이버

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `email.driver` | `smtp` \| `http` \| `lua` \| `none` | `none` | 어느 싱크가 전달하는가 | 그 외 거부 |
| `email.from` | string | `""` | envelope sender | `@`를 포함해야 한다. **`smtp` 드라이버에 필수** |
| `email.to[]` | []string | `[]` | 기본 수신자 | 각각 `@`를 포함해야 한다. **`smtp` 드라이버는 최소 하나 필수** — 주소가 아닌 주소는 전달 시점에 실패하고, 그 시점은 알림이 보고하려던 인시던트 도중이다 |
| `email.smtp.addr` | string | `""` | `host:port` | `smtp`에 필수이며 `host:port`로 파싱돼야 한다 |
| `email.smtp.username` | string | `""` | SMTP 사용자 | **비밀번호와 함께 필수** |
| `email.smtp.key_env` / `key_file` | secret ref | — | SMTP 비밀번호, 평범한 비밀 참조로 (§0.3) | development 밖의 인라인 리터럴은 다른 곳과 마찬가지로 거부 |
| `email.smtp.starttls` | bool | `false` | 연결을 업그레이드 | ⚠️ **username을 설정하고 `starttls: false`면 거부된다.** Go의 SMTP 클라이언트는 loopback이 아닌 서버에 암호화되지 않은 연결로 `PLAIN`을 보내지 않으므로, 그러지 않으면 전달 시점에 실패한다; 로드에서 말해 주는 쪽이 더 싼 발견이다 |
| `email.smtp.tls_skip_verify` | bool | `false` | 검증 불가한 인증서 수용 | 사설 릴레이를 위한 의도적 다운그레이드 |
| `email.smtp.timeout` | duration | `10s` | 전달별 타임아웃 | 음수 거부 |
| `email.smtp.helo` | string | `""` | dorang이 자신을 알리는 이름 | — |
| `email.http.url` | string | `""` | webhook 엔드포인트 | `http`에 필수. host가 있는 `http`/`https`여야 한다. **여기서는 쿼리 문자열이 괜찮다** — shadow 참조와 달리 URL이 요청 경로와 결합되지 않고 통째로 쓰이며, 호스팅 webhook은 흔히 토큰을 쿼리에 싣는다 |
| `email.http.key_env` / `key_file` | secret ref | — | 서명 비밀값 | ⚠️ **선택이 아니라 필수.** 전달은 본문에 대한 HMAC-SHA256으로 서명되고, 비밀값이 없는 수신자는 진짜 전달과 위조를 구별할 수 없다. 페이로드가 예산과 쿼터 상태를 싣기 때문에, 서명 없는 webhook은 URL이 닿는 순간 정보 유출이다 |
| `email.http.timeout` | duration | `10s` | 전달별 타임아웃 | 음수 거부 |
| `email.http.headers{}` | map | `{}` | 추가 요청 헤더 | — |

⚠️ **`driver: lua`는 `on_email` 훅을 요구한다.** `extensions.lua.enabled: false`인 채로, 또는 `on_email`을
빠뜨린 `hooks` 목록과 함께 선택하면 **거부된다** — 조용히 아무것도 보내지 않는 메일 시스템이 이 절이
다루는 바로 그 실패이기 때문이다.

### 19.3 페이로드에 비밀값 없음

알림은 이름 붙은 필드들로 조립되고, 하나가 메시지에 도달하기 전에 서로 독립적인 셋이 동의해야 한다:

1. 필드 이름이 그 이벤트의 허용목록에 있다. 그 외는 드롭되고 계수된다 — 호출자가 실수로 필드를 추가할 수
   없다.
2. 필드 이름이 거부 목록에 없다. 어떤 허용목록이 뭐라 하든 무관하게.
3. 값이 키 자료처럼 *보이지* 않는다. 프로바이더 토큰처럼 시작하거나, 길고 불투명한 값은 redaction 마커로
   대체되고 계수된다.

그래서 `key_created` 알림은 키 id와 라벨을 싣고 **키를 실을 수 없다.** 호출자가 시도했든 아니든. 양방향
모두 테스트로 단언된다 — 호출자 쪽에서 하나, 렌더러 쪽에서 하나.

---

## 21. `priority_mapping`

```yaml
priority_mapping:
  classes: {realtime: 0, interactive: 2, batch: 10}
  emit:
    vllm:   {field: priority}
    openai: {field: service_tier, map: {realtime: priority, interactive: default, batch: flex}}
    header: X-Request-Priority
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `classes.<name>` | int | `{realtime: 0, interactive: 2, batch: 10}` | canonical 스케일. **낮을수록 긴급** | 음수 거부. 클래스를 하나라도 설정하면 기본 맵 전체를 대체하므로 셋 다 명명할 것 |
| `emit.header` | string | `X-Request-Priority` | 네이티브 priority 필드를 이해하지 못하는 백엔드에 보내는 헤더 | 비어 있고 백엔드 매핑도 없으면 거부: 네이티브 필드가 없는 백엔드도 헤더는 받는다 |
| `emit.<backend>.field` | string | — | 백엔드의 네이티브 priority 필드 | 빈 값 거부 |
| `emit.<backend>.map.<class>` | string | — | 필드가 숫자가 아닌 백엔드를 위한 클래스 → 와이어 값 번역 | `classes`에 선언되지 않은 클래스 거부, 빈 값도 거부 |

`emit` 아래의 `header` 외 키는 백엔드 이름이고, 백엔드 블록에는 `field`와 `map`만 올 수 있다. 그 외는 줄
번호와 함께 거부된다.

### 20.1 두 self-hosted 엔진은 priority를 **반대 방향**으로 정렬한다

⚠️ vLLM은 **낮은** 값을 먼저 스케줄한다. SGLang은 기본적으로 **높은** 값을 먼저 한다. 같은 필드 이름, 같은
타입, 어느 쪽이든 `200`, 그리고 응답 어디에도 무엇이 일어났는지 드러나지 않는다. 위 canonical 맵을 그대로
SGLang에 보내면 **batch가 realtime을 앞지른다** — degradation이 아니라 반전이다.

canonical 스케일은 낮을수록 긴급으로 유지되고 adapter가 내림차순 엔진에 대해 부호를 뒤집는다. SGLang 쪽
권장 해법은 `--schedule-low-priority-values-first`로, SGLang을 vLLM과 일치시켜 하나의 맵이 둘을 서빙하게
한다. 상세: [VLLM.ko.md](VLLM.ko.md) §1.2, [SGLANG.ko.md](SGLANG.ko.md) §0.1과 §3.

priority에 의존하기 전에 알아야 할 둘:

- **두 엔진 모두 기본값에서 조용히 무시한다.** vLLM은 `--scheduling-policy priority`, SGLang은
  `--enable-priority-scheduling`이 필요하다. 없으면 값이 엔진까지 흘러가 소비자가 없다. dorang은 오퍼레이터
  선언을 기록하고, 동작한다고 가정하는 대신 미검증 emission을 결정에 표시한다.
- **내림차순 엔진용 부호 반전은 dorang의 전체 스케일을 비양수 반직선에 놓는다.** *공유* 엔진에서는 순진한
  양수 priority를 보내는 동거 테넌트가 realtime 포함 dorang의 모든 트래픽을 앞지른다.

### 20.2 클라이언트는 긴급성을 주장할 수 없다

클라이언트가 보낸 priority 힌트는 **기본적으로 무시된다.** 허용 범위로 클램프하는 것이 초기 규칙이었고
충분히 안전하지 않다: priority는 공유 capacity에 대한 청구권이므로, 호출자가 설정할 수 있으면 결국 모든
호출자가 가장 긴급한 값을 설정한다 — 악의가 아니라 공짜이고 도움이 되어 보이기 때문에. 그러면 스케일이
정보를 싣지 않고, 건드리지 않은 호출자들이 벌을 받는다. 클램프는 self-elevation의 폭을 제한할 뿐 유인을
제거하지 못한다.

비대칭은 의도적이다: **오퍼레이터는 긴급성을 부여할 수 있고, 호출자는 주장할 수 없다.** 부여는 priority
클래스 이름 둘로 된 `range`와 함께 쓰는 `capacity.principals.<id>.client_priority: allow`이고, 힌트는
`X-Request-Priority`로 다닌다. 양쪽 모두 구현돼 있다: 부여가 없는 principal은 힌트가 드롭되고 그 드롭이
보고된다.

힌트 드롭은 조용하지 않고 `x-dorang-dropped-params`에 보고된다.

---

## 21a. `compat`

[COMPATIBILITY.ko.md](COMPATIBILITY.ko.md)가 명명하는, 오퍼레이터가 고를 수 있는 세 가지 divergence.
셋 다 거기에 오퍼레이터 설정 가능이라고 문서화돼 있는데, 이 절이 생기기 전까지 스키마에는 `compat:`
블록 자체가 없었다 — 그래서 그중 무엇이든 설정한 파일은 unknown-key 에러로 로드에 실패했고, 문서는
존재하지 않는 노브를 서술하고 있었다.

```yaml
compat:
  legacy_headers: false         # 대상 프록시의 응답 헤더 이름을 함께 낸다
  usage_chunk_choices: stub     # stub | empty
  anthropic_total_tokens: true  # true | false
```

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `legacy_headers` | bool | `false` | 대상 프록시의 응답 헤더 철자를 dorang 자신의 것과 **나란히** 추가한다(COMPATIBILITY §7.7a가 이름별로 열거한다). 이름을 바꾸지도, 무엇을 없애지도 않는다 | 깨지는 것은 없다. 다른 벤더의 이름을 내보내는 것이므로 기본이 꺼짐이다. 컷오버 동안 켜고 아무것도 읽지 않게 되면 끌 것. 이것이 막는 실패는 조용하다: `x-litellm-response-cost`를 읽는 exporter는 그 헤더가 오지 않기 시작해도 에러를 내지 않고 **0**을 보고한다 |
| `usage_chunk_choices` | string | `stub` | COMPATIBILITY §3.3. `stub`은 대상 프록시의 `"choices":[{"index":0,"delta":{}}]`, `empty`는 엄격한 OpenAI의 `[]`. **둘 다 서빙된다.** | 대상 프록시에 맞춰 쓰인 클라이언트는 usage 청크에서 `choices[0].delta`를 읽으므로 `empty`에서 인덱스 에러가 난다. 그래서 기본은 `stub`으로 둔다. `empty`를 고르면 같은 패밀리 스트림이 바이트 릴레이 fast path에서도 빠진다 — 그 경로에서 usage 청크는 업스트림 자신의 바이트이고 dorang이 그 형태를 고르지 않으므로, 보장의 대가가 프레임당 디코드 한 번이다. 그 둘 외의 값은 기본값으로 떨어지지 않고 미지의 값으로 로드에서 거부된다 |
| `anthropic_total_tokens` | bool | `true` | COMPATIBILITY §6.8. 비스트리밍 Anthropic 응답에 스펙에 없는 `usage.total_tokens`를 추가한다. **일부** 대상 프록시 빌드가 그것을 낸다. **두 값 모두 서빙된다**; `false`가 엄격한 벤더 형태다 | 어느 쪽이든 깨지지 않는다 — 그 멤버는 추가적이고, 그 패밀리의 모든 SDK가 모델링하지 않은 멤버를 견딘다. 비스트리밍 응답에만 적용된다: **스트리밍**된 메시지는 어느 설정에서도 `usage.total_tokens`를 싣지 않으며, 그 비대칭 *자체*가 §6.8이지 스위치의 구멍이 아니다. **기본값은 당신의 기존 게이트웨이에 대한 주장이 아니다:** 2026-07에 측정한 한 배포는 `{"input_tokens":68,"output_tokens":8}`를 내고 `total_tokens`는 내지 않았으므로, 그것과 바이트 패리티를 원하면 `false`다. 바이트 단위로 같아야 하는 컷오버 전에는 기존 게이트웨이 자신의 `/v1/messages` 응답을 확인할 것 |

셋 중 둘은 예전에 기본값이 아닌 값에서 **거부**됐다. 그 이유는 남겨 둘 값어치가 있다. 그것이 §23의
요점이기 때문이다: DESIGN §17.1은 "로드되고 검증되고 아무것도 읽지 않는 설정"을 이 저장소의 지배적
결함으로 지목하며, 그것을 저지르지 않는 방법은 설정을 배선하거나 거부하는 둘뿐이다. `internal/wire`는
두 쌍의 두 형태를 모두 쓰인 이래로 인코딩하고 있었고 `internal/config`는 두 철자를 모두 파싱하고
있었지만, 어떤 표현식도 한쪽을 다른 쪽에 잇지 않았다 — 그래서 로더가 거부했고, 그 거부 메시지가 빠진
홉의 이름을 댔다.

그 홉은 이제 존재한다: `backend.Call.UsageChunkChoices`와 `backend.Call.AnthropicTotalTokens`가 리로드
가능한 dispatch 상태에서 요청마다 채워진다. 세 키 모두 두 값 모두에서 서빙되고, 거부는 그것이 서술하던
간극과 함께 사라졌다. 여전히 거부되는 것은 이 스키마가 모르는 값이다 — `usage_chunk_choices: stubb`는
로드 에러다. 오타를 조용히 기본값으로 처리하면 오퍼레이터가 요청하지 않은 형태를 건네는 것이 되기 때문이다.

---

## 22. 참조 무결성

모든 상호 참조가 로드 시 이름으로, YAML 경로와 함께 검사된다:

| 참조 | 해결 대상 |
|---|---|
| `providers[].capacity_group` | `capacity.provider_groups`의 키 |
| `credentials[].provider` | `providers[].name` |
| `credentials[].capacity_group` | `capacity.credential_groups`의 키 |
| `capacity.models[].provider` | `providers[].name` |
| `key_rotation.providers.<name>` | `providers[].name` |
| `key_rotation.…keys[].id` | 선언된 크리덴셜을 가리키면 그 크리덴셜의 프로바이더가 일치해야 한다 |
| `key_rotation.…keys[].capacity_group` | `capacity.credential_groups`의 키 |
| `models[].class` | `classes`의 키, **그리고** 모델이 그 멤버여야 한다 |
| `models[].deployments[].provider` | `providers[].name` |
| `models[].deployments[].credentials[]` | 같은 프로바이더의 `credentials[].id` |
| `aliases.<name>` | `models[].name`, 다른 alias 불가, 모델 이름 자체와 충돌 불가 |
| `classes.<name>[]` | `models[].name`, 중복 불가 |
| `pricing.rules[].match.provider` / `.credential` | 선언된 프로바이더 / 크리덴셜 |
| `passthrough.routes[].provider` | `providers[].name` |
| `priority_mapping.emit.<backend>.map.<class>` | `priority_mapping.classes`의 키 |

---

## 23. 환경 변수

| 변수 | 읽는 곳 | 이름 설정 위치 | 비고 |
|---|---|---|---|
| `DORANG_CONFIG` | `dorang`, `dorangctl` | — | `--config`의 기본값. 설정하면 경로가 **명시적**이 되므로 없는 파일이 에러가 된다 |
| `DORANG_MASTER_KEY` | 서버 | `server.master_key_env` | out-of-band 관리 크리덴셜 |
| `DORANG_KEY_PEPPER` | 서버, `dorangctl key` | `server.key_pepper_env` | HMAC pepper. 미설정이면 DB 옆에 생성 — §2 |
| `DORANG_DATABASE_URL` | 서버 | `storage.postgres.url_env` | `driver: postgres`일 때만 |
| `DORANG_REDIS_URL` | 서버 | `cluster.redis_url_env` | 절대 읽히지 않는다; `capacity_mode: shared-redis`는 로드에서 거부된다 |
| `DORANG_CATALOG_PATH` | 서버, `dorangctl` | — | PATH 구분 모델 카탈로그 레이어, **마지막**에 적용. 없는 레이어는 skip이 아니라 **에러**: 조용히 떨어진 레이어야말로 카탈로그가 답하려는 질문 그 자체다 |

`dorangctl key create`와 `dorangctl migrate`는 서버와 같은 방식으로 pepper를 해결한다. 다른 pepper로 키를
발급하는 CLI는 서버가 검증할 수 없는 키를 발급하게 된다.

`DORANG_STATE_DIR`은 선행 `~`로 쓰인 모든 state 경로를 재배치한다 — 데이터베이스, 트레이스 spool, 생성된
key pepper, batch 블롭, shadow 리포트. 컨테이너 이미지는 그것을 자신이 볼륨으로 선언한 디렉터리로
설정한다. 그것이 없으면 `~`는 서빙 사용자의 홈이고, 이미지의 `nonroot` 사용자에서 그것은 `/home/nonroot`
— 볼륨 밖, 컨테이너의 쓰기 가능 레이어이며 재시작에 사라진다. 생성된 pepper를 잃으면 그 아래에서 발급된
모든 api 키가 검증 불가능해진다.

**그 디렉터리를 누가 소유하느냐가 게이트웨이의 기동 여부를 결정한다.** 이미지는 uid `65532`로 돌고,
자기가 소유하지 않은 디렉터리 안에는 데이터베이스도 spool도 pepper도 만들 수 없다. 증상은 기동 시
`mkdir /var/lib/dorang/…: permission denied`이고, `restart: unless-stopped` 아래에서는 크래시 루프이며,
이미지에는 셸이 없어 안에서 고칠 방법이 없다. 세 가지 마운트 중 무엇을 쓰느냐가 이 일이 일어날 수
있는지를 가른다:

| 마운트 | 소유권 | 해야 할 일 |
|---|---|---|
| **네임드 볼륨** (`-v dorang-state:/var/lib/dorang`) | Docker가 이미지에서 소유권까지 함께 시드하고, 이미지는 그 디렉터리를 `65532` 소유로 싣고 있다 | 없음 |
| **바인드 마운트** (`-v /srv/dorang:/var/lib/dorang`) | 항상 호스트 디렉터리의 것. Docker는 바인드 마운트에 아무것도 복사하지 않는다 | 첫 기동 **전에** 호스트에서 `chown 65532:65532 /srv/dorang` |
| **Kubernetes 볼륨** | `emptyDir`은 world-writable이라 그대로 되고, **PersistentVolumeClaim**은 안 된다 | 파드 `securityContext`에 `fsGroup: 65532` — `deploy/kubernetes.yaml`이 그렇게 한다 |

실패하던 것은 네임드 볼륨 경우였고, 진단하기 가장 어려운 방식으로 실패했다: Docker는 볼륨을 **생성할
때만** 시드하므로, 한 번 밖에서 chown해 둔 운영자는 그 뒤로 다시는 보지 못하고 재현할 수도 없다.

---

## 24. 스키마가 받지만 이 빌드가 작동시키지 않는 키

이 절의 모든 것은 검증되고 로드되며 아무 효과가 없다. 설정했다고 믿는데 아무 일도 하지 않는 상한은 상한이
없는 것보다 나쁘므로 이름으로 열거한다.

> ⚠️ **이 절의 두 표는 2026-07-29에 통째로 다시 유도됐다.** 그 전까지 한국어 미러는 영문 §23.1a가
> 기록한 "지난 패스 이후 닫힌 것" 표를 아예 갖고 있지 않았고, 그래서 아래 열 개가 넘는 행이 이미 배선된
> 설정을 무력하다고 말하고 있었다 — `key_rotation.strategy`, `observability.prometheus`,
> `DORANG_STATE_DIR`, `capacity.*.max_queue`/`.max_queue_wait`, `quota_urgency`,
> `client_priority`, `metering_degraded`, 인라인 `notional_rate`, 그리고 Lua 인터프리터. 무력하다고
> 잘못 적힌 설정은 무력한 설정보다 나쁘다: 운영자가 쓸 수 있는 제어를 쓰지 않게 만든다. 지운 것이 아니라
> 무엇이라고 적혀 있었고 무엇이 참인지 함께 남긴다.
>
> **이번 패스에서 §23.1b와 §23.2도 이 미러에 처음 들어왔다.** 그리고 몇 군데에서는 영문과 Go 소스가
> 어긋났다 — 그럴 때 판정한 것은 코드다. `key_rotation.strategy`, `metering_degraded`,
> `admin.Config.Pricing`이 그런 항목이고, 각각 배선된 상태로 여기 적혀 있다.

### 23.1 받아들여지고 무력한 것

| 키 | 상태 |
|---|---|
| `metric: max_concurrent` 또는 `max_queue`인 `models[].deployments[].limits[]` | `rpm`과 `tpm`만 소비되며, 크리덴셜별 쿼터로 (§10.2) |
| `routing.prefix.checkpoints` | 체인은 로그 간격으로 잘린다; `fixed`는 검증만 되고 아무것도 선택하지 않는다 |
| `models[].deployments[].stream_timeout` | 비스트리밍 타임아웃만 업스트림 호출에 닿는다 |
| `key_rotation.providers[].affinity_group` | 읽히지 않고, 검증되지도 않는다 |
| `key_rotation.providers[].stickiness.scope` | 읽히지 않는다; 세션 키는 `routing.sticky.key`에서 온다 |
| `cluster.redis_url_env` | 아무것도 다이얼하지 않는다. Redis 클라이언트가 어디에도 만들어지지 않으므로 `capacity_mode: shared-redis`는 로드에서 거부되고, 이 키는 LiteLLM에서 가져온 설정이 파싱되도록 남겨 둔 것뿐이다 |
| `observability.otlp_endpoint` | exporter가 연결돼 있지 않다 |
| `observability.log_level`, `.log_format` | 어떤 로거도 읽지 않는다; 진단은 임베더가 주는 `Logf` 훅으로 나간다 |

`internal/config/consumed_test.go`가 이 목록을 산문이 아니라 실행 가능한 상태로 들고 있다: 소비자
없는 설정을 추가하면 실패하고, 목록에서 지우지 않은 채 배선해도 실패한다. Go 필드 이름이 너무 흔해서
검색으로 볼 수 없는 행들 — `Enabled`, `Endpoint`, `Interval`, `Drop`, `Scope`, `StreamTimeout` — 은
보지 못하며, 그래서 그 행들은 여기에 손으로 적혀 있다. 이 표가 가드로 대체되지 않고 가드 옆에 존재하는
이유의 전부가 그것이다.

> **`providers[].metrics`는 이 표에 두 번에 걸쳐 올라 있었고, 이제는 대신 거부된다.** 그 행은 처음에
> *"endpoint와 enabled 플래그는 읽히고 poll interval은 아니다"*라고 적혀 있었는데 양쪽 절반 모두 거짓이었다:
> `internal/config` 밖에 셋 중 무엇에 대한 reader도 없었다. 가드는 그것을 반박할 수 없었다. `Enabled`,
> `Endpoint`, `Interval`이 전부 트리의 다른 곳에 나오는 이름이기 때문이다 — `consumed_test.go` 자신의
> doc 주석이 경고하는 그 공허함이, 필드 하나가 아니라 블록 전체에 대해 발화한 것이다. 정정한 뒤에도 그
> 행은 깨끗하게 로드되고 아무것도 하지 않는 블록을 서술했고, §6.2의 엔진 함정들이 살아 있는 경로에 대한
> 조언처럼 읽혔다. **이제 그 블록은 로드 에러다**(§23.2). 이 절이 계속 주장해 온 처분이고, 가드가 닿을 수
> 없는 처분이다. 무력한 채로 두는 것의 대가를 볼 것: 같은 블록에 대한 두 개의 틀린 진술이, 같은 표에서,
> 두 번의 패스에 걸쳐 있었다.

> **`providers[].params.drop`은 배선되어서, 그 이웃은 거부되어서 이 표에서 내려왔다.** 그 행은
> *"§10.3의 두 노브는 변환 경로에 닿지 않는다 — 무엇이 떨어지는지는 kind 자신의 capability 집합만
> 정한다"*라고 적혀 있었고, 둘 다에 대해 참이었으며 둘 다에 대해 틀린 처분이었다. `params.drop[]`은
> 인코더 앞에서 요청에 적용되고 지워진 이름을 `x-dorang-dropped-params`로 보고한다
> (`internal/canonical/dropparam.go`, `internal/backend/provider.go`). `params.drop_unsupported:
> false`는 이제 로드 에러다(§23.2). capability 집합과 drop 목록은 애초에 같은 질문에 대한 답이 아니다:
> capability는 **kind**가 표현할 수 있는 것이고, drop은 **이 업스트림 인스턴스 하나**가 거부하는 것이며,
> 후자는 오퍼레이터만 아는 사실이다.

### 23.1a 지난 패스 이후 닫힌 것

아래는 모두 위 표에 있던 것이다. 정본과 각 항목의 근거는 영문 [CONFIG.md](CONFIG.md) §23.1a.

| 키 | 지금 하는 일 |
|---|---|
| `providers[].params.force_stream`, `providers[].params.store_false` | **신규.** `/responses`만 서빙하고 `stream: true` + `store: false` 외에는 거부하는 호스트(ChatGPT Codex 표면)의 계약을, 호출자마다 400으로 만나는 대신 프로바이더당 한 번 진술한다. `/responses` 라우팅은 카탈로그 kind의 `responses_only` 플래그가 결정한다(단일 출처: 어댑터 선택과 다른 kind에서 이 두 키를 거부하는 기동 검사가 같은 선언을 읽는다). 비스트리밍 호출자도 버퍼된 답 하나를 받는다 — 스트림을 중립 형태로 수집해 호출자 자신의 인코더로 렌더링하기 때문. 첫 초안의 적대적 검토에서 수집한 답이 곁길로 렌더링되고 있었다: 호출자 프로토콜과 무관하게 항상 chat JSON, 서빙 모델 기록·도구 인자 검사·§10.5b 변환 생략, 중간 오류 문구가 가려지지 않은 채 통과하고 재시도 가능 코드라 과금된 생성을 이웃 배포에서 반복할 뻔했다. 전부 공유 `convert` 꼬리로 옮겼고 각각 되돌리면 실패하는 이름 있는 테스트가 있다 |
| `providers[].params.api_version`, kind `azure` | **신규.** azure kind는 통째로 거부되고 있었다("이 빌드에 어댑터 없음"). 필요한 것은 OpenAI 어댑터 옆의 딱 두 가지 — 경로와 헤더 — 였고 이제 갖췄다: 기본은 통합 `/openai/v1` 마운트, 운영자가 값을 대면 레거시 배포별 경로와 필수 `api-version`, 그리고 `api-key` 자격증명 헤더. kind의 `api`가 `azure-openai`가 되어 `max_tokens_field`, T1 경로(임베딩·오디오·이미지·모더레이션), 스트리밍 레거시 completions가 이것을 OpenAI 형태로 다룬다. `api_version`은 어댑터 하나만 읽는 다른 설정처럼 kind 밖에서는 거부 |
| `providers[].params.project`, `providers[].params.location`, kind `vertex`, `oauth.format: gcp-service-account` | **신규.** vertex kind는 통째로 거부되고 있었다("프로젝트 id, location, 그리고 프로젝트 범위 경로의 Google 서비스 계정 OAuth 교환이 필요한데 설정이 어느 것도 들고 있지 않다"). 이제 들고 있다. 어댑터는 location의 리전 호스트 위 프로젝트 범위 경로의 Gemini 어댑터이고, 자격증명은 OAuth 자격증명의 형태에 맞춰 넣은 서비스 계정 키 파일 — `format: gcp-service-account` — 이라 헬스, 선제 갱신, 401 재시도, 계정 헤더가 그대로 적용된다: 파일은 읽기 전용(갱신이 운영자의 키를 덮어쓰지 않음), 비밀이 아닌 `private_key_id`가 refresh token 자리를 대신해 갱신 가능함을 알리고, 첫 사용 시 RS256 assertion을 만들어 교환한다(RFC 7523, 동기). 토큰이 전혀 없는 저장소는 첫 사용에 발급하고, 만료된 토큰은 선제 갱신 루프와 401 경로의 몫이다. `refresh.token_url`은 파일의 `token_uri`를 덮어쓰는 선택 항목이며 `client_id`가 필요 없다. 서명·발급자·대상·scope를 검증하는 가짜 Google 토큰 엔드포인트와 가짜 Vertex 호스트에 대해, 운영자 파일에서부터 고정 |
| `providers[].params.region`, `.access_key_id`, `.session_token`, kind `bedrock` | **신규.** bedrock kind는 거부되고 있었다("SigV4 요청 서명과 Converse 요청 형태가 필요한데 둘 다 dorang에 없다"). 둘 다 이제 있다: `internal/wire/bedrock`이 Converse(두 채굴 소스가 이름 댄 중립 형태)와 그 이진 이벤트 스트림을 말하고, `internal/backend`의 SigV4 서명기 — AWS 자신의 `get-vanilla` 테스트 벡터로 고정 — 가 `send()`의 새 `requestSigner` 시임을 통해 만들어진 요청에 서명한다(자격증명이 헤더가 아니라 요청 전체에 대한 서명이므로). secret access key가 자격증명, region·access key id는 프로바이더 파라미터, session token은 선택적 비밀 참조. 실계정 측정은 아님 |
| `providers[].usage_probe` | **배선됨.** §6.2의 fetcher들은 구현되고 테스트되고 아무것도 import하지 않았다 — `internal/probe`는 트리에 importer가 0개였다 — 그래서 모든 쿼터 결정이 dorang 자신의 트래픽에 대한 dorang 자신의 시야로 돌았고, §6.2가 존재하는 이유가 바로 그것으로는 부족하다는 것이다. `internal/app`이 이제 활성 프로바이더마다 prober를, 게이트할 규칙이 있는 크리덴셜마다 tracker를 만들고 요청 경로 밖에서 폴링한다. prober가 없는 `fetcher`는 존재하는 것들을 이름으로 대는 기동 거부다. 스키마가 그것을 검사할 수 없고, 조용히 영영 보고하지 않는 프로브가 이것이 닫은 상태이기 때문이다. `allowances[]`가 그 곁의 신규이며, 보고된 퍼센트를 규칙이 게이트할 수 있는 수치로 바꾸는 것이 그것이다(§6.1a) |
| `capacity.*.max_queue`, `capacity.principals.<id>.max_queue`, `.max_queue_wait` | 배선됨. `max_queue`는 축별 대기 큐를 제한하고 초과를 거부하며, `max_queue_wait`는 principal의 요청 하나가 기다릴 수 있는 시간을 제한한다 — 그것을 쓰는 것은 **핀된** 요청이다. 배치는 면제(§11.1) |
| 모든 그룹·`global`·`models[]`·`principals`의 `capacity.*.rpm`, `.tpm` | **거부되며**, 동작하는 자리를 이름으로 알려 준다. 용량 축은 동시 예약을 세고, 레이트는 시간 윈도우가 필요하며 그것은 `internal/quota`의 것이다 |
| `key_rotation.strategy` | 배선됨. 네 이름 모두 선호 크리덴셜을 고른다 (`internal/app/build.go`의 `router.ParseRotation`) |
| `observability.prometheus` | 배선됨. `false`는 `/metrics` 라우트를 없애고, 그러면 다른 미서빙 라우트처럼 501을 답한다 |
| `observability.metrics.public` | 신규. `/metrics`는 인증을 요구하며 기본적으로 master 크리덴셜을 요구한다; 이 키가 그것을 의도적으로 연다 |
| `capacity.principals.<id>.client_priority` + `range` | 신규. §10.5의 예제가 이제 로드된다 (`GrantsClientPriority`) |
| `models[].strategy[]`의 `quota_urgency` | 허용값이며 배선됐다. `internal/quota`의 Ranker가 `Deps.Urgency`를 먹인다 (`router/wiring_test.go`의 `TestUrgencySourceIsTheQuotaRanker`) |
| `metering_degraded` | 배선됨. `internal/metrics/collect_meter.go`가 `dorang_metering_degraded`와 `dorang_metering_degraded_reason`을 게시한다 |
| `pricing.rules[].class: notional_rate` (`source`·`as_of` 포함) | 신규. §8.5의 클래스가 더 이상 카탈로그 전용이 아니다 |
| `pricing.rules[].rates.cache_read` | 신규 철자, `cached_read`와 나란히. 두 파일에서 같은 컴포넌트를 뜻한다 |
| `pricing.rules[].rates.images` | 조립 에러가 아니라 이제 **로드** 에러다 — 예전에는 `config lint`를 통과한 뒤 서버가 뜨지 못하게 했다 |
| `pricing.rules[].rates.request`, `.characters` | **신규가 아니라 수정.** 검증은 통과하고 "rate does not belong to unit per_1m_tokens"로 조립을 거부했다: 인라인 스키마에는 `unit:` 키가 없고 아무 값도 쓰지 않았으므로, 그것이 광고한 모든 비토큰 요율이 `rates.images` 결함이었다 — 린트는 통과하고 서버는 뜨지 않았다. 단위는 이제 규칙이 값을 매기는 컴포넌트에서 유도되고, 두 단위를 섞는 규칙은 린트에서 거부된다 |
| `pricing.rules[].rates.seconds` | `compute_seconds`와 `audio_seconds`로 **대체됨**. 키 하나가 두 개의 청구 수량을 이름 짓고 필드에 든 것 — 요청의 벽시계 시간 — 에 값을 매겼으므로, 녹음에 청구하는 전사 벤더에 dorang 자신의 지연이 청구됐다. §13.1b |
| `key_ref` | **거부됨**, `key_env`와 `key_file`을 이름으로 대며 |
| `providers[].prefix_ttl`, `models[].deployments[].prefix_ttl` | 신규. affinity 수명은 백엔드별이다. 그것이 그 값이 모델링하는 것이기 때문이고, vLLM과 SGLang에 정직한 값은 `until_evicted`다 |
| `DORANG_STATE_DIR` | 배선됨. 앞머리 `~`가 붙은 모든 state 경로가 여기로 해석되므로 이미지의 기본값이 자신이 선언한 볼륨 안에 착지한다 (`internal/config/secret.go`) |
| `compat.legacy_headers`, `.usage_chunk_choices`, `.anthropic_total_tokens` | 셋 다 끝에서 끝까지 배선됨. `legacy_headers`는 동작하는 소비자를 가진 `server.Options` 필드로 존재했고 **어떤 설정도 거기에 닿을 수 없었다** — `internal/app`이 그것을 설정한 적이 없어서, COMPATIBILITY §7.7의 미러링 주장은 코드에 대해 참이고 모든 배포에 대해 거짓이었다. 뒤의 둘은 그 전에는 비기본값에서 **거부**됐다: 인코더는 쓰인 이래로 두 형태를 모두 냈고 어떤 설정도 하나를 고를 수 없었다. §21a |
| `server.read_header_timeout`, `.read_timeout`, `.idle_timeout` | 신규, 끝에서 끝까지 배선됨. 셋 다 동작하는 소비자를 가진 `server.Options` 필드로 존재했고 어떤 설정도 닿을 수 없었으므로 모든 배포가 내장된 30s/2m/2m로 돌았다. `none`은 상한을 없앤다 — 앞단 프록시가 이미 하나를 강제하는 배포를 위한 값이다. `read_header_timeout`보다 짧은 `read_timeout`은 린트에서, **그리고** 기동에서 로드 에러다. §2, §2.1 |
| `auth.miss_budget.rate`, `.burst` | 신규, 끝에서 끝까지 배선됨. 같은 형태다: 아무도 발급하지 않은 키에 대한 스토어 조회를 제한하는 토큰 버킷은 동작하고 있었고 그 두 `auth.Config` 필드에는 YAML에서 오는 경로가 없었다. 음수 `rate`는 상한을 없애고, 음수 `burst`는 거부된다. `internal/auth`가 그것을 부재로 읽기 때문이다. §5 |

### 23.1b 반대 방향: Go에는 있고 YAML에는 없는 노브

§23.1은 스키마가 받고 아무것도 작동시키지 않는 설정이다. **이것은 그 거울**이고, 사례를 더 많이 만들어
온 쪽이 이쪽이다: `server.Options`나 `auth.Config`의 필드가 동작하는 소비자와 기본값을 갖고 있는데 설정
파일에서 오는 경로가 없는 경우. §23.1a가 그 닫힌 목록이다 — `compat.legacy_headers`,
`compat.usage_chunk_choices`, `compat.anthropic_total_tokens`, `server.max_body_bytes`, 세 연결
deadline, 그리고 miss-budget 노브 둘이 거기 있고, 각각 같은 이유로 보이지 않았다.

**`internal/config`의 재발 방지 가드는 이 방향을 아예 볼 수 없다.**
`TestEveryConfiguredFieldIsReadSomewhere`는 `Config` 타입을 걸으며 각 필드가 읽히는지 묻는다. `Config`에
없는 노브는 걸어지지 않는다. 그리고 추가되는 것들에 대해서도 가드는 식별자 **이름**으로 매칭하므로
`ReadTimeout`, `IdleTimeout`, `Rate`, `Burst`는 아무것도 그것들을 소비하지 않는 동안 내내 소비된 것으로
보고했다 — 그 가드 자신의 doc 주석이 열거하는 공허함이다. 지금 이것들을 붙들고 있는 것은 `config.LoadBytes`를
조립된 게이트웨이에 통과시키는 `internal/app`의 설정별 행동 테스트다.

위의 다섯을 닫은 sweep이 발견했고 여전히 열려 있는 것들. 다음 것을 우연히 마주치는 대신 찾아보라고 여기
적어 둔다:

| 필드 | 하는 일 | 어떤 배선이 필요한가 |
|---|---|---|
| `server.Options.ReplayBudgetBytes` | **프로세스 전역** 본문 보유 예산(§15.4): fallback이 재생할 수 있도록 진행 중인 모든 요청이 함께 붙들 수 있는 양. `0`은 256 MiB, 음수는 보유를 끄고 모든 요청을 재생 불가로 표시한다 | `server.replay_budget_bytes` 키. 요청별 상한은 설정 가능한데, 그것을 맞춰 잡아야 할 프로세스 전역 상한은 설정 불가능하다. (§2의 `max_body_bytes` 행은 예전에 "`metering.max_replay_bytes`에 맞춰 잡으라"고 말했다 — **존재한 적 없는 키**여서 아무도 돌릴 수 없는 손잡이의 이름을 대고 있었다. 그 문장은 §2에서 사라졌지만 키는 여전히 없다) |
| `auth.Config.StoreTimeout` | 크리덴셜 스토어 조회 하나의 상한; 2s | `auth.store_timeout` 키. 그것은 데이터베이스의 성질이고, 정확히 그렇게 설정 *가능한* `auth.revocation.store_latency`와 같다 — 다른 revocation 상한이 필요할 만큼 스토어가 느린 배포는 다른 조회 상한도 필요하다 |
| `auth.Config.RehashQueue` | 비동기 `legacy_sha256` → `dorang_v1` 업그레이드 큐의 깊이; 256 | `auth.rehash_queue` 키. legacy 마이그레이션 중에만 문제가 되는데, 그때가 정확히 fleet이 보는 모든 키를 업그레이드하는 시점이다. 오버플로는 `dorang_auth_rehash_dropped_total`로 보인다 |
| `auth.Config.MasterKeyID` | 로그와 계측에서 master principal의 이름; `"master"` | 아마 필요 없다 — 그것은 상한이 아니라 라벨이다. 판단을 다시 내리지 않도록 적어 둔다 |

> **영문 §23.1b는 이 표 아래에 다섯 번째로 `admin.Config.Pricing`을 열어 둔 것으로 적고 있으나, 그것은
> 더 이상 참이 아니다.** `internal/app/admin.go`가 `Pricing: &adminPricer{d: a.dispatch}`를 설정하므로
> `POST /admin/pricing/preview`와 `POST /spend/calculate`는 이제 답한다. 그 전까지 둘은 모든 배포에서
> `dependency_unavailable`("pricing engine")을 답했고, 그동안 `dorangctl price`와 요청 경로는 같은 엔진으로
> 가격을 잘 매기고 있었다 — DESIGN §8.4의 "엔진 하나, 답 하나"가 세 표면 중 둘에서만 거짓이었다.
> `adminPricer`는 카탈로그를 붙잡아 두지 않고 dispatch 상태에서 읽는다: 오퍼레이터가 `SIGHUP`을 보내는
> 주된 이유가 편집한 `pricing.catalog`를 집어 들게 하는 것이고, preview는 그것이 먹혔는지 확인하러 가는
> 자리이기 때문이다.

### 23.2 설계돼 있고, 스키마에 있고, 로드에서 거부되는 것

로드되고 아무것도 하지 않는 키는 거부되는 키보다 나쁘다: 그것을 쓴 오퍼레이터는 그것이 먹혔다고 믿는다.
아래는 파싱은 된다 — 거부가 오타처럼 읽히는 대신 키 이름을 대고 무엇을 쓸지 말할 수 있도록 — 그리고
검증에서 실패한다.

| 키 | 거부의 내용과, 그 능력이 실제로 사는 곳 |
|---|---|
| `providers[].metrics` — **블록 전체**, `enabled`·`endpoint`·`interval` 중 무엇이든 | **§12.4의 백엔드 메트릭 스크레이프에는 수집기가 없다. R17은 만들어지지 않았다.** 저장소 안의 무엇도 그 엔드포인트를 가져오지 않는다. 거부는 동작하는 대안을 이름으로 대고, 그 대안은 비용이 없다: `least_busy`와 `highest_tps`는 구현돼 있고 스크레이프에 의존하지 않는다 — 앞의 것은 요청이 선점할 축에 대한 이 게이트웨이 자신의 실시간 capacity 점유율로, 뒤의 것은 완료된 요청에서 측정한 초당 출력 토큰으로 순위를 매기며(§7.5a), 둘 다 "아직 표본 없음"을 0이 아니라 의견 없음으로 다룬다. `models[].strategy`에 이름을 적을 것. 스크레이프가 **더해 주는** 것은 엔진 자신의 큐 깊이와 KV 캐시 사용률이고, 그것이 더 나은 신호인 경우는 정확히 하나다: dorang을 거치지 않은 트래픽도 함께 받는 self-hosted 백엔드. 플래그뿐 아니라 endpoint 단독으로도 거부한다. 플래그를 끈 채 endpoint를 쓰는 것이 변경을 단계적으로 넣는 방식이고, 거기에 침묵으로 답한 것이 이 블록이 문서화 패스 두 번을 살아남은 방법이기 때문이다 |
| `credentials[].key_ref`와 그 밖의 모든 `*_ref` | 비밀값 resolver가 실려 있지 않다. `key_env`나 `key_file`을 쓸 것. 파일을 쓰거나 변수를 내보내는 볼트 에이전트면 둘 다 만족한다 |
| `capacity.*.rpm`, `.tpm` | rate는 gauge가 아니다. 배포별 rate는 `models[].deployments[].limits[]`, 호출자별 rate는 그 키 자신의 `rpm_limit`/`tpm_limit` |
| `cluster.capacity_mode: shared-redis` | 프로토콜은 실려 있고 그것을 말하는 클라이언트가 없다. 공표된 초과가 똑같이 `0`인 `shared-pg`를 쓸 것 |
| `metering.numeric.enabled: false` | 수치 회계는 끌 수 없다. `metering.trace.sample_rate`를 낮출 것 |

### 23.2a 설계돼 있고 스키마에 아예 없는 것

| 설계 절 | 없는 것 |
|---|---|
| §6.1 `quotas:` — `cost_usd`나 `tokens_total`에 대한 롤링 `5h`/`daily`/`weekly`/`monthly` 윈도우와 `on_exhaust` | **최상위 `quotas:` 블록이 없다.** 이 빌드가 만드는 유일한 쿼터 규칙은 `deployments[].limits[].rpm`/`.tpm`에서 파생된 롤링-분 요청·토큰 카운터다 |
| §6.4 `budget:` — `period`/`limit_usd`/`on_exceed` 블록 | **최상위 `budget:` 블록이 없다.** 예산은 API 키별이며 `dorangctl key create --budget-usd`로 설정하고 내구 원장으로 강제된다 |
| §7.4a2 `stickiness.pin_on_state` | 스키마에 없고 — 그것이 옳다: 핀은 설정이 아니라 요청에서 추론된다 (§7.1) |
| §11.5 "Lua 훅" — `extensions.lua.dir` 아래 | 훅 지점, 상한, fail-open/fail-closed 규칙, 비밀값 없는 view는 구축돼 있고, `extensions.lua.dir`이 담는 것은 total 정책 언어인 `*.policy`다. **그 디렉터리 아래의 `.lua` 파일은 여전히 로드 에러다** — 안에 나타나는 것을 무엇이든 실행하는 디렉터리는 코드 실행 프리미티브이고 §11.5는 그것을 거부한다. Lua 자체가 없는 게 아니다: 한 절 건너 `filters.plugins[].path`로, 스캔이 아니라 이름으로 선언된다. §16 |

> **이 행은 이번 패스 전까지 "Lua 인터프리터가 없다"고 적혀 있었고, `a0d5871` 이후로 거짓이었다.**
> `go.mod`가 `github.com/yuin/gopher-lua v1.1.2`를 요구하고, `internal/luaext`는 명령어 상한·스택
> 상한·과금되는 할당을 갖춘 샌드박스 VM이며, `internal/app/extensions.go`가 그것을 설정에서 만들고
> `internal/app/filter.go`가 선언된 모든 플러그인을 요청 경로에서 컴파일한다. `94de856`·`bb67140`·`241f73b`가
> 그것을 굳혔다. 옳게 살아남은 것은 이 행이 이제 하는 더 좁은 주장 — `extensions.lua.dir`의 로드 에러 —
> 이고, 그 둘을 합친 것이 오류의 전부였다: 거부의 대상은 *디렉터리를 스캔하는 것*이지 언어가 아니다.

### 23.3 `--check`가 잡지 못하는 것

- 린트하는 기계에는 설정돼 있고 서빙하는 기계에는 없는(또는 그 반대인) `key_env` 변수(`--check`에서 비밀값
  실패는 경고로 낮아진다).
- *내용*이 잘못된 `storage.postgres.url_env`.
- 구문이나 이름 오류가 아닌 가격에 관한 그 무엇도: `rates.images`와 출처가 빠진 `notional_rate` 규칙은
  이제 둘 다 로드에서 거부된다.
- 프로바이더에 `base_url`이 없어 조용히 삭제되는 `passthrough.routes[]` 항목.
- 업스트림이 존재하는지, 응답하는지, `kind`가 주장하는 프로토콜을 말하는지에 대한 그 무엇도.

---

## 25. 세 가지 실제 설정

같은 바이너리, 같은 스키마가 전 구간에서 동작한다. 배포 모델이 아니라 설정을 바꿔 확장한다.

| | 노트북 | 팀 | 엔터프라이즈 |
|---|---|---|---|
| 저장소 | SQLite 내장 | PostgreSQL | PostgreSQL, 파티션 |
| 조정 | 없음 | 없음 | Redis 또는 PostgreSQL 리스 |
| 노드 | 1 | 1–2 | N, 리더 선출 유지보수 |
| 용량 정확성 | 정확 | 정확 | 정확(공유) 또는 상한 있는 초과(리스) |
| 계측 | 인프로세스, 샘플링 | 전체 원장 | 원장 + 롤업 + 보존정책 |
| **필수 의존성** | **0** | 1 | 2 |
| 목표 처리량 | 수백 req/s | 수천 req/s | 노드당 10k+ req/s |

### 24.1 노트북 — 의존성 0

프로세스 하나, 파일 하나, 설치할 것 없음. 바이너리가 정적(SQLite 드라이버가 순수 Go)이라 `scratch`에서도
돈다.

```yaml
version: 1

server:
  listen: "127.0.0.1:4100"
  env: development            # 랩톱이고, 여기서 커밋되는 것은 없다
  request_timeout: 600s

storage:
  driver: sqlite
  sqlite: {path: ~/.dorang/dorang.db}

providers:
  - name: local-vllm
    kind: vllm
    base_url: http://127.0.0.1:8000/v1
    timeout: 300s
    max_concurrency: 8

credentials:
  - {id: local, provider: local-vllm, key_env: LOCAL_VLLM_KEY}

capacity:
  principals: {default: {max_concurrent: 8}}

models:
  - name: local-large
    class: chat-large
    strategy: [prefix_sticky, least_busy]
    deployments:
      - {provider: local-vllm, upstream_model: my-model, credentials: [local]}

classes: {chat-large: [local-large]}

metering:
  trace: {store_messages: truncated, sample_rate: 1.0}
  spool: {dir: ~/.dorang/spool, max_bytes: 256MiB}

observability: {log_level: debug, log_format: text}
```

이 티어에서 중요한 지점:

- `env: development`가 실험 중 `key: sk-…`를 인라인으로 쓰게 해 준다. 이 파일을 서버로 가져가지 말 것;
  §1.2는 기계가 아니라 파일을 위해 존재한다.
- `DORANG_KEY_PEPPER` 미설정도 여기서는 괜찮다: pepper가 `dorang.db` 옆에 생성되어 둘이 함께 다닌다.
  **함께 백업하지 않으면** 발급한 모든 키가 검증 불가능해진다.
- `sample_rate: 1.0`은 전부 보관한다. ~1 req/s에서 하루 ~44 MB 발췌.
- 게이트웨이가 보고하는 무엇이든 믿기 전에 vLLM 플래그를 설정할 것 — 특히
  `--enable-prompt-tokens-details`, 없으면 캐시된 prompt 토큰이 정가로 과금된다.
  [VLLM.ko.md](VLLM.ko.md) §5.

### 24.2 팀 — 의존성 1

PostgreSQL, 노드 1–2개, 공유 계정 풀, 실제 비용 회계. 조정 서비스는 아직 없다: 두 번째 노드가 트래픽을
서빙하는 peer가 아니라 standby인 한 `cluster.enabled: false`가 옳다 — `local` 회계의 peer 둘은 §1.1의
초과를 누락으로 얻는 것이다.

```yaml
version: 1

server:
  listen: ":4100"
  env: production
  master_key_env: DORANG_MASTER_KEY
  key_pepper_env: DORANG_KEY_PEPPER      # 반드시 설정: 노드 둘, 키 데이터베이스 하나
  request_timeout: 600s
  shutdown_grace: 45s                    # p99보다 길게, 롤링 재시작이 조용하도록

storage:
  driver: postgres
  postgres: {url_env: DORANG_DATABASE_URL, max_conns: 32}

providers:
  - name: cloud-a
    kind: openai
    base_url: https://api.example-cloud-a.invalid/v1
    timeout: 120s
    max_concurrency: 24
    capacity_group: cloud-a-pool
    # usage_probe 없음: OpenAI는 조직 단위 과거 지출을 Admin 키 뒤에 공표하는데,
    # 그것은 "이 키에 얼마가 남았는가"와 다른 주제다 — 그래서 `openai` fetcher는
    # 의도적으로 없고, 그 이름을 대면 기동 에러다 (§6.1).
  - name: plan-a
    kind: glm
    base_url: https://api.example-plan-a.invalid
    timeout: 180s
    max_concurrency: 14
    capacity_group: plan-a-pool

credentials:
  - {id: acct-1,   provider: cloud-a, key_env: CLOUD_A_KEY_1, capacity_group: acct-1}
  - {id: acct-2,   provider: cloud-a, key_env: CLOUD_A_KEY_2, capacity_group: acct-2}
  - {id: plan-a-1, provider: plan-a,  key_file: /etc/dorang/secrets/plan-a.key,
     capacity_group: plan-a-account}

capacity:
  provider_groups:
    cloud-a-pool: {max_concurrency: 6}     # 두 계정 합쳐서
    plan-a-pool:  {max_concurrency: 14}
  credential_groups:
    acct-1: {max_concurrency: 3}           # 계정당, 모든 모델
    acct-2: {max_concurrency: 3}
    plan-a-account: {max_concurrency: 7}
  models:
    # (프로바이더, upstream 모델)당. 위의 plan-a-account와 겹쳐 적용되고 더 좁은
    # 쪽이 이긴다 — 아래 주석 참고.
    - {provider: plan-a, model: model-x, max_concurrency: 7}
    - {provider: plan-a, model: model-y, max_concurrency: 7}
  principals:
    default: {max_concurrent: 16}
  global: {max_concurrency: 64}
  interactive_reserve: 0.3

key_rotation:
  providers:
    cloud-a:
      stickiness: {scope: session, on_capacity: spill}
      keys:
        - {id: acct-1, key_env: CLOUD_A_KEY_1, max_concurrency: 3, capacity_group: acct-1}
        - {id: acct-2, key_env: CLOUD_A_KEY_2, max_concurrency: 3, capacity_group: acct-2}

models:
  - name: model-x
    class: chat-large
    strategy: [prefix_sticky, lowest_cost, least_busy]
    deployments:
      - {provider: plan-a,  upstream_model: model-x,       credentials: [plan-a-1], weight: 10, priority: 0}
      - {provider: cloud-a, upstream_model: model-x:cloud, credentials: [acct-1, acct-2],
         weight: 5, priority: 1, limits: [{metric: rpm, value: 600}, {metric: tpm, value: 400000}]}
  - name: model-y
    class: chat-small
    deployments:
      - {provider: plan-a, upstream_model: model-y, credentials: [plan-a-1]}

aliases: {model-large: model-x, model-small: model-y}
classes: {chat-large: [model-x], chat-small: [model-y]}

pricing:
  catalog: /etc/dorang/pricing.yaml
  currency: USD

metering:
  trace: {store_messages: truncated, truncate_chars: 512, sample_rate: 0.2, daily_byte_budget: 4GiB}
  spool: {dir: /var/lib/dorang/spool, max_bytes: 2GiB}
  flush_interval: 250ms

observability: {log_level: info, log_format: json}
```

이 티어에서 중요한 지점:

- **`DORANG_KEY_PEPPER`를 명시적으로 설정할 것.** 노드 둘에 생성된 pepper면 각자 자기 것을 만들고 한쪽이
  발급한 키를 다른 쪽이 검증할 수 없다.
- ~50 req/s에서 `sample_rate: 0.2`는 발췌를 하루 ~2.2 GB 대신 ~440 MB로 유지한다.
- plan-a 숫자가 축이 존재하는 이유의 형태다: 모델마다 자기 7이 있다 — 하지만 위의 `plan-a-account`가 그
  키를 모든 모델 통틀어 7로 묶고 더 좁은 축이 이기므로, 여기서 실제로 도달 가능한 값은 **14가 아니라
  총 7**이다. 모델 둘이 정말 14에 이르려면 credential group을 빼야 하고, 그것이
  `testing/providers/dual-key.yaml.tmpl`이 하는 일이자 적어 둔 내용이다. 모델별 축의 키는
  `(프로바이더, upstream 모델)`이고 credential이 아니므로, `plan-a`에 키를 하나 더 붙여도 자기 버킷이
  새로 열리지 않고 같은 두 버킷에서 가져간다.
- `usage_probe`가 dorang을 거치지 않은 트래픽까지 쿼터에 반영시킨다(§6.1). 이 파일의 두 프로바이더에는
  prober가 없어서 쓰이지 않았다. prober가 있는 셋은 `zai`, `anthropic`(OAuth 구독, §11.2b), `deepseek`이고,
  보고된 퍼센트를 규칙이 게이트할 수 있는 수치로 바꾸는 `allowances`는 §6.1a에 있다. 만료 임박 쿼터
  보고가 조금이라도 의미를 갖게 만드는 것도 그것이다.

### 24.3 엔터프라이즈 — 의존성 2

N 노드, 정확한 공유 capacity, 파티션 원장, 강한 샘플링.

```yaml
version: 1

server:
  listen: ":4100"
  env: production
  master_key_env: DORANG_MASTER_KEY
  key_pepper_env: DORANG_KEY_PEPPER
  request_timeout: 600s
  shutdown_grace: 60s

storage:
  driver: postgres
  postgres: {url_env: DORANG_DATABASE_URL, max_conns: 64}

cluster:
  enabled: true
  node_id: ""                    # 오케스트레이터에서 노드마다 주입(예: pod 이름), 또는
                                 # 비워 둘 것: 파생된 id는 충돌할 수 없다. 이미 쓰이는
                                 # id로 뜬 두 번째 프로세스는 기동을 거부한다.
  redis_url_env: DORANG_REDIS_URL
  capacity_mode: shared-pg       # 정확하다; 핫패스에 +1 RTT, 의도적으로
  min_leasable: 16

providers:
  - name: fleet-vllm
    kind: vllm
    base_url: http://vllm.internal:8000/v1
    timeout: 300s
    max_concurrency: 256
    capacity_group: fleet
    # 여기에 `metrics:`는 없다: §12.4의 스크레이프는 로드에서 거부된다 (§6.2, §23.2).
    # 아래의 least_busy는 이 프로바이더 축에 대한 dorang 자신의 점유율로 순위를
    # 매기며, 정확하고 폴이 필요 없다.
  - name: cloud-a
    kind: openai
    base_url: https://api.example-cloud-a.invalid/v1
    timeout: 120s
    max_concurrency: 128
    capacity_group: cloud
    # usage_probe 없음: OpenAI는 조직 단위 과거 지출을 Admin 키 뒤에 공표하는데,
    # 그것은 "이 키에 얼마가 남았는가"와 다른 주제다 — 그래서 `openai` fetcher는
    # 의도적으로 없고, 그 이름을 대면 기동 에러다 (§6.1).

credentials:
  - {id: fleet-1, provider: fleet-vllm, key_file: /run/secrets/fleet.key, capacity_group: fleet-acct}
  - {id: acct-1,  provider: cloud-a,    key_env: CLOUD_A_KEY_1, capacity_group: acct-1}
  - {id: acct-2,  provider: cloud-a,    key_env: CLOUD_A_KEY_2, capacity_group: acct-2}

capacity:
  provider_groups: {fleet: {max_concurrency: 256}, cloud: {max_concurrency: 96}}
  credential_groups:
    fleet-acct: {max_concurrency: 256}
    acct-1: {max_concurrency: 48}
    acct-2: {max_concurrency: 48}
  principals: {default: {max_concurrent: 32}}
  global: {max_concurrency: 512}
  interactive_reserve: 0.3

models:
  - name: chat-primary
    class: chat-large
    strategy: [prefix_sticky, lowest_cost, least_busy]
    deployments:
      - {provider: fleet-vllm, upstream_model: my-70b, credentials: [fleet-1], weight: 10, priority: 0}
      - {provider: cloud-a, upstream_model: model-x, credentials: [acct-1, acct-2],
         weight: 3, priority: 1, limits: [{metric: rpm, value: 3000}, {metric: tpm, value: 4000000}]}

classes: {chat-large: [chat-primary]}

pricing: {catalog: /etc/dorang/pricing.yaml, currency: USD}

metering:
  trace: {store_messages: hash, sample_rate: 0.01, daily_byte_budget: 8GiB}
  spool: {dir: /var/lib/dorang/spool, max_bytes: 8GiB}
  flush_interval: 250ms

observability: {log_level: warn, log_format: json}

priority_mapping:
  classes: {realtime: 0, interactive: 2, batch: 10}
  emit:
    vllm: {field: priority}
    header: X-Request-Priority
```

이 티어에서 중요한 지점:

- **`node_id`는 노드별로 구별되고 안정적이어야 한다.** 빈 값은 *프로세스*마다 파생하므로 재시작한 노드가
  자기 리스를 회수하지 못하고 만료를 기다린다. 오케스트레이터에서 pod 이름 등으로 주입할 것. 다만 빈 값은
  충돌할 수 없고, 충돌하는 id 쪽이 훨씬 나쁜 실패다: 이미 쓰이는 id로 뜬 두 번째 프로세스는 **기동을
  거부한다**. 그 거부가 생기기 전에는 두 프로세스가 모두 리드했고 리더 전용 작업이 전부 두 번 돌았다
  (§4, `node_id`는 모든 노드에서 서로 달라야 한다).
- `capacity_mode: shared-pg`는 핫패스에 왕복 하나를 물리며, 그 프로파일의 p99 목표는 5 ms다. 정확한
  회계의 값이고, `leased`는 그것을 공표된 초과 `block × (nodes − 1)`과 맞바꾼다. `shared-redis`가 왕복당
  더 쌌겠지만 그것은 **거부된다**: 이 빌드는 그 모드의 프로토콜을 싣고 클라이언트는 싣지 않으며, 스토어를
  쓰면서 `shared-redis`를 보고하는 coordinator는 정확히 이 결정의 값을 잘못 매기게 된다.
- `min_leasable: 16`은 `shared-pg`에서 무력하고 `leased`로 바꾸는 순간 하중을 받는다 — 그 시점에
  파일의 16 미만 `max_concurrency`가 전부 거부된다.
- ~2 k req/s에서 `store_messages: hash` + `sample_rate: 0.01`은 전체 발췌의 하루 ~88 GB 대신 ~880 MB를
  일일 바이트 예산에 **청구한다** — 두 자릿수 차이를 만드는 것은 샘플링이다. 실제로 **보관되는** 양은
  거기서 또 훨씬 적다: 예산은 읽은 발췌 길이로 청구되고 그것을 대체하는 다이제스트는 고정 71바이트
  (`sha256:` + hex 64)이므로, 저장 컬럼은 청구된 값의 약 7분의 1이다. 예산은 청구량으로, 디스크는
  보관량으로 잡을 것.
- `key_ref`는 **이 빌드에서 거부된다**: 그렇게 선언된 크리덴셜은 외부 resolver가 생기기 전까지 로드되고
  쓸 수 있는 비밀값이 없었을 것이다. 오늘은 `key_env`나 `key_file`을 쓸 것.
- vLLM `metrics` 스크레이프는 **이 빌드에 없고 `providers[].metrics`는 거부된다**(§6.2). `least_busy`는
  dorang 자신의 용량 점유율로 순위를 매기므로 폴이 필요 없다. R17이 만들어질 때를 위해 함정은 §6.2에 남겨 두었다.

---

## 함께 보기

- [OPERATIONS.ko.md](OPERATIONS.ko.md) — 설치·운영·메트릭 읽기, 그리고 문제가 생겼을 때.
- [MIGRATION.ko.md](MIGRATION.ko.md) — 기존 게이트웨이의 설정·크리덴셜 임포트와 shadow 컷오버.
- [DESIGN.ko.md](DESIGN.ko.md) §4 — 스키마의 근거.
- [VLLM.ko.md](VLLM.ko.md) §5, [SGLANG.ko.md](SGLANG.ko.md) §8 — 위의 모든 것이 말한 대로 의미하기 전에
  self-hosted 백엔드에 필요한 플래그.
