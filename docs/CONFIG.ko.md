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
| Decimal | `"0.0000025"`, `"20.00"`, `"5"` | **텍스트로 보관**되어 정확히 파싱된다. 가격은 절대 이진 부동소수를 거치지 않는다(§8.3). 따옴표로 감쌀 것 — 아니면 YAML이 float를 준다 |
| Path | `~/.dorang/dorang.db` | 선행 `~`는 프로세스 사용자 홈으로 확장된다 |

### 0.3 비밀값

비밀값은 이 파일에 절대 나타나지 않는다. 모든 키 자료 필드는 참조이며, 넷 중 **정확히 하나**만 설정한다:

```yaml
key_env:  NAME             # 프로세스 환경에서 읽는다
key_file: /path/to/key     # 파일에서 읽고 후행 개행을 제거한다
key_ref:  vault:secret/…   # 그대로 기록해 외부 resolver에 넘긴다
key:      literal          # server.env가 "development"일 때만 허용
```

하나도 설정하지 않으면 에러. 둘 이상이면 에러. 설정되지 않았거나 빈 변수를 가리키는 `key_env`도 에러이고,
읽을 수 없거나 빈 `key_file`도 에러다.

`key_ref`는 **기록되지 가져오지 않는다.** 이 빌드에는 resolver가 없으므로, `key_ref`만으로 선언된
크리덴셜은 로드되고 검증되며 요청 시점에 쓸 수 있는 비밀값이 없다.

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

**규칙.** `cluster.enabled: true` + `capacity_mode: local`은 기동을 거부한다. `shared-redis`,
`shared-pg`, `leased`를 쓸 것.

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

**규칙.** `key:`는 `server.env: development`일 때만 허용된다.

**왜.** 설정 파일은 커밋되고, 티켓에 복사되고, 채팅창에 붙여지고, 템플릿 엔진이 렌더링하는 유일한
산출물이다. 그 각각이 `key_env` 참조에는 없는 노출 경로다. 규칙의 요지는 리터럴 자체가 불안전하다는 게
아니라, 비밀값을 담을 *수도 있는* 파일은 그것을 담은 것처럼 영원히, 모두가 다뤄야 한다는 것이다 — 그리고
그 규율은 새벽 3시 인시던트와 만나면 살아남지 못한다.

`development`는 규칙 때문에 랩톱을 못 쓰게 만들지 않기 위해 존재한다. 보안 자세가 아니다: 그 밖에는
아무것도 완화하지 않으므로, 프로덕션 배포의 키 처리를 우회하려고 `env`를 바꾸면 정확히 하나를 얻고 감사
기록을 잃는다.

### 1.3 종료일 없는 legacy 키 해싱

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
| `pricing.rules[].rates.images` | **조립.** 린트는 통과 | `pricing rule X: the images component is not priced by this build` |
| `pricing.rules[].class: notional_rate` | **로드.** 인라인 클래스는 셋만 허용 | `"notional_rate" is not a known value` — notional 규칙은 카탈로그 파일에 둘 것 (§13.3) |
| `shadow.mode`를 켜고 `sample_rate` 미설정 | 절대. 돌면서 **아무것도** shadow하지 않는다 | 게이트 판정 `no_data`, 빈 리포트, 그리고 빈 리포트가 증거로 읽힌다 (§18) |
| `storage.driver: postgres` + URL 변수 미설정 | **조립.** 린트는 변수 *이름*만 확인 | 기동 시 스토어 open 실패 |

---

## 2. `server`

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `listen` | string | `:4100` | 리슨 주소 `host:port` | 빈 값 거부. 커맨드라인 `--listen`이 우선하며, 한 파일로 두 인스턴스를 띄우는 방법이다 |
| `env` | `production` \| `development` | `production` | 인라인 리터럴 비밀값 허용 여부(§1.2) | 프로덕션에서의 `development`는 커밋된 키를 유효한 설정으로 만든다 |
| `master_key_env` | string | `DORANG_MASTER_KEY` | 관리 크리덴셜을 담은 변수 이름. out-of-band로 상수 시간 비교되며 **행으로 저장되지 않는다** | 빈 값 거부. 변수 자체가 미설정이면 명시적 opt-out이 없는 한 authenticator가 구성을 거부한다 — 관리 인증을 잃는 것은 타이핑해서 명시해야 한다. 그러지 않으면 임포트된 DB만 읽는 게이트웨이가 조용히 관리자를 잃는다 |
| `key_pepper_env` | string | `DORANG_KEY_PEPPER` | `dorang_v1` 키 해싱용 HMAC pepper 변수 이름 | 빈 값 거부. **변수가 미설정이면 pepper가 한 번 생성되어 SQLite DB 옆에 기록된다.** 의존성 0의 노트북 티어를 유지시키지만, pepper가 DB 파일과 함께 다닌다 — 변수를 설정하지 않은 다중 노드 배포에서는 각 노드가 서로 검증할 수 없는 키를 발급한다 |
| `request_timeout` | duration | `600s` | 전체 요청 deadline. capacity 예약 만료(`request_timeout + 30s`)도 설정하므로 누수된 goroutine이 슬롯을 영구 점유할 수 없다 | 0 이하 거부. 너무 짧으면 긴 생성이 잘리고, 너무 길면 멈춘 업스트림이 그 창 내내 예약을 붙든다 |
| `shutdown_grace` | duration | `30s` | drain이 진행 중 요청을 기다리는 시간, 그리고 별도로 drain 이후 해체가 걸릴 수 있는 시간 | 음수 거부. p99보다 짧으면 롤링 재시작이 가시적 에러가 된다: 프로세스가 하드 종료하고 종료 코드 1과 `drain grace expired with requests still in flight`를 낸다 |

---

## 3. `storage`

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

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `enabled` | bool | `false` | 다중 노드 동작: 리더 선출, 리스 조정, §1.1 가드 | fleet에서 `false`로 두면 각자 자기 상한을 세는 독립 게이트웨이 N개가 된다 — §1.1이 거부하는 바로 그 초과를 설정 대신 누락으로 얻는다 |
| `node_id` | string | `""` | 리스 행에서 이 프로세스의 이름 | 빈 값은 **프로세스마다** 하나를 파생한다. 재시작한 노드가 자기 리스를 회수하지 못하고 만료를 기다린다. 클러스터의 모든 노드에 설정할 것 |
| `redis_url_env` | string | `DORANG_REDIS_URL` | Redis URL 변수 이름 | `capacity_mode: shared-redis`일 때 비어 있으면 안 된다 |
| `capacity_mode` | `local` \| `shared-redis` \| `shared-pg` \| `leased` | `local` | capacity **및 쿼터**의 노드 간 조정 방식. 정확도 어휘는 둘이 아니라 하나 | §1.1 참조. `enabled: true`와 `local`은 기동 거부 |
| `min_leasable` | int | `16` | `leased` 모드가 노드에 나눌 최소 상한 | 0 이하 거부. `leased`에서 파일 어디든 이 값 미만의 `max_concurrency`는 거부된다 — 한 자릿수 상한은 유용하게 나눌 수 없고, 나눌 수 있는 척하면 대부분의 노드가 블록 0을 쥔 fleet이 된다 |

`leased`에서 검증기는 `providers[].max_concurrency`, `capacity.provider_groups[].max_concurrency`,
`capacity.credential_groups[].max_concurrency`, `capacity.models[].max_concurrency`,
`key_rotation.providers[].keys[].max_concurrency`를 `min_leasable`과 대조한다. 위반마다 경로와 값을 명시한다.

> **롤링 윈도우는 `local` 모드에서만 정확하다.** 롤링 5시간 허용량에는 자연스러운 period 경계가 없으므로,
> 공유·리스 모드는 그것을 epoch 정렬 그리드에 키잉한다. 실제 근사이며 숨기지 않고 기록한다; 설계는 아직
> 이를 다루지 않는다.

---

## 5. `auth`

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `legacy.enabled` | bool | `false` | 마이그레이션 창 동안 unsalted 기존 다이제스트 허용 | §1.3 참조 |
| `legacy.until` | date | `""` | 창의 끝. `legacy.enabled`가 true면 필수 | 파싱 불가하거나 이미 지난 날짜는 기동 거부 |
| `rehash_on_use` | bool | `true` | legacy 검증 성공 시 `dorang_v1`으로의 비동기 업그레이드 예약 | 끄면 마이그레이션이 스스로 끝나지 않고, 창이 닫힐 때 키가 동작을 멈춘다 |

두 다이제스트가 항상 계산되고 비교 대상이 branchless하게 선택되므로 키의 스킴이 타이밍으로 관측되지
않는다. 캐시 경로의 인증은 354 ns, 무할당. 암호 연산이 지배하며 스냅샷 조회 자체는 55 ns다.

---

## 6. `providers`

업스트림 서비스 하나. `name`과 `kind`는 필수.

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `name` | string | — | 프로바이더의 정체성. **프로바이더 식별은 여기와 `deployments[].upstream_model`에서만 오고, 모델 문자열 파싱에서 오지 않는다** | 빈 값·중복 거부. 프로바이더를 참조하는 모든 것이 이 이름과 대조된다 |
| `kind` | string | — | 한 단어로 wire adapter, 능력 기본값, 프롬프트 캐시 스킴, 리즈닝 제어 형태를 선택. 모델 카탈로그를 통해 해결되므로 오버레이가 이 빌드가 모르는 kind를 추가할 수 있다 | 빈 값 거부. 알 수 없는 kind는 로드에서 거부되지 **않는다** — 카탈로그 레이어로 해결되며 `dorangctl catalog explain <kind> <model>`이 어느 레이어가 답했는지 알려준다 |
| `base_url` | string | `""` | 업스트림 루트. **적어 넣은 URL에 버전 세그먼트가 없으면 dorang이 붙인다** — §6.0 참고 | 비어 있으면 다이얼할 수 없고, `kind`가 선언한 기본값으로 되돌아간다. `base_url`이 없는 프로바이더의 패스스루 라우트는 아무 데도 가리키지 않게 두는 대신 **조용히 삭제된다** |
| `timeout` | duration | `0` | 프로바이더별 요청 타임아웃. 배포가 override 가능 | 0이면 `server.request_timeout`으로 떨어진다 |
| `max_concurrency` | int | `0` | `route` capacity 축 — 이 프로바이더 자신의 상한 | 음수 거부. `0`은 0짜리 상한이 **아니라** 이 축이 제약하지 않음을 뜻한다 |
| `capacity_group` | string | `""` | 여러 프로바이더가 하나의 업스트림 풀을 공유할 때의 `provider_group` 축 멤버십 | `capacity.provider_groups`에 선언되지 않은 그룹은 이름과 함께 거부 |
| `params.drop_unsupported` | bool | `true` | kind가 표현할 수 없는 파라미터를 전달하지 않고 걸러낸다 | 끄면 클라이언트가 본 적 없는 업스트림 `400`이 노출된다. 기존 클라이언트들이 만들어진 대상 프록시가 조용히 드롭하며, 그것이 기본값 이유다 |
| `params.drop[]` | []string | `[]` | 이 파라미터들을 무조건 제거 | 빈 항목이나 공백만 있는 항목은 거부 |
| `params.set{}` | map | `{}` | 호출자를 덮어쓰며 무조건 주입 | ⚠️ 안전 메커니즘을 끌 수 있는 레버다. 여기에 `truncation: auto`나 `truncate_prompt_tokens`를 넣으면 프로바이더 전체에서 context-window 폴백이 키로 삼는 바로 그 `400`이 억제된다 — [EXTENSIONS.ko.md](EXTENSIONS.ko.md) §A.3a |
| `params.default{}` | map | `{}` | 호출자가 보내지 않았을 때만 주입 | `set`보다 안전하지만 같은 두 파라미터로 같은 해를 끼칠 수 있다 |
| `retry.max_attempts` | int | `2` | 프로바이더 내 재시도, §12의 폴백 체인과 별개 | 음수 거부 |
| `retry.backoff` | `exponential` \| `linear` \| `constant` | `exponential` | 재시도 간격 | 그 외 거부 |
| `retry.base` | duration | `500ms` | 첫 재시도 간격 | 음수 거부 |
| `usage_probe.enabled` | bool | `false` | 프로바이더 자신의 잔여 쿼터 조회 | 없으면 쿼터가 로컬 계측만 되고, 로컬 계측은 dorang을 거치지 않은 트래픽만큼 과소 계산한다 |
| `usage_probe.fetcher` | string | `""` | 어느 프로바이더별 fetcher를 쓸지 | 프로브가 켜져 있으면 필수 |
| `usage_probe.interval` | duration | `0` | 폴 간격 | 음수 거부. 폴은 최대 한 간격만큼 낡는다 — §6.1 |
| `metrics.enabled` | bool | `false` | `least_busy` / `highest_tps`용 백엔드 메트릭 스크레이프 | §6.2 |
| `metrics.endpoint` | string | `""` | 스크레이프 URL | `metrics.enabled`가 true면 필수 |
| `metrics.interval` | duration | `0` | 스크레이프 간격 | — |

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

### 6.2 백엔드 메트릭에는 엔진별 함정이 있다

`metrics`를 켠다고 `least_busy`가 옳아지지 않는다. 두 self-hosted 엔진 모두 잘못 명명됐거나 죽었거나
리셋되는 메트릭이 있다:

- vLLM의 `kv_cache_usage_perc`는 이름과 달리 0–1 분수이고, `--disable-log-stats`를 주면 `/metrics`가
  **시리즈 0개로 200**을 반환한다 — 유휴와 구별되지 않으면서 정반대를 뜻한다. [VLLM.ko.md](VLLM.ko.md) §3.
- SGLang의 `/metrics`는 `--enable-metrics`가 없으면 **404**이며 그것이 정직한 실패다. 그리고
  `sglang:cache_hit_rate`는 매 decode 리포트마다 `0.0`으로 하드 리셋되어 게이지가 대부분의 시간을 0에서
  보낸다. [SGLANG.ko.md](SGLANG.ko.md) §4.3.

숫자로 라우팅하기 전에 해당 엔진 절을 읽을 것.

---

## 7. `credentials`

프로바이더에서의 인증 정체성 하나 — **쿼터와 동시성이 실제로 붙는 단위**이자, 상태를 갖는 대화에 대해서는
대화 상태가 붙는 단위다.

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `id` | string | — | 크리덴셜의 정체성. **비밀이 아니다**: 라우팅 결정, 메트릭, 헤더, 에러에 나타난다 | 빈 값·중복 거부 |
| `provider` | string | — | 어느 프로바이더에서 인증하는가 | 선언된 프로바이더여야 한다. 다른 프로바이더의 크리덴셜을 열거한 배포는 이름과 함께 거부 |
| `auth` | `key` \| `oauth` | `key` | 이 크리덴셜이 어떻게 인증하는가 | 그 외 값은 거부. `oauth` 블록 없는 `auth: oauth`도, `auth: oauth` 없는 `oauth` 블록도 거부 |
| `key_env` / `key_file` / `key_ref` / `key` | secret ref | — | 비밀값 출처 (§0.3) | 0개 또는 2개 이상 거부 |
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

### 8.1 축

| 축 | 설정 위치 | 세는 단위 |
|---|---|---|
| `route` | `providers[].max_concurrency` | 프로바이더 하나 |
| `provider_group` | `capacity.provider_groups` | 업스트림 풀을 공유하는 여러 프로바이더 |
| `model` | `capacity.models[]` | **(키, 모델) 쌍 하나** |
| `credential_group` | `capacity.credential_groups` | **계정 하나, 모든 모델** |
| `key` | `key_rotation.providers[].keys[].max_concurrency` | 키 하나 |
| `principal` | `capacity.principals` | 호출자 하나, **API 키 id**로 키잉 |
| `global` | `capacity.global` | 프로세스(공유 모드에서는 클러스터) |

요청이 필요로 하는 모든 축이 **하나의 임계 구역**에서 전량 또는 전무로 예약된다. 부분 점유가 존재하지
않으므로 대기자가 한 슬롯을 쥔 채 다른 슬롯을 기다리는 일이 없고, 데드락이 구조적으로 불가능하다. 대기는
축별 FIFO에 타깃 wakeup이며, 상한 1·7·32에 대해 대기자 1000명에서 **grant당 정확히 1.00 wakeup**으로
측정됐다. broadcast라면 release당 `O(대기자)`다. 전체 7축 acquire 경로는 455 ns, 할당 1회.

⚠️ **"부분 보유 금지"의 대가는 아직 열려 있는 liveness 구멍이다.** 포화된 두 축이 필요한 대기자가 두 큐의
head에 앉은 채 둘이 동시에 비는 순간을 영영 만나지 못할 수 있다. 데드락도 추월 문제도 아니므로 aging
메커니즘이 닫을 수 없고, 아직 설계되지 않은 소프트 예약 프로토콜이 필요하다. 리스크 W8로 기록돼 있으며
측정 가능하다: 두 축이 포화인 채로 절대 이기지 못하는 대기자.

### 8.2 키

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `provider_groups.<name>.max_concurrency` | int | `0` | provider-group 상한 | 음수 거부. **`0`은 0짜리 상한이 아니라 이 축이 제약하지 않음을 뜻한다.** 프로바이더가 참조하는데 여기 선언되지 않은 그룹은 거부 |
| `credential_groups.<name>.max_concurrency` | int | `0` | 모든 모델에 걸친 계정당 상한 | 동일. "모델 무관 동시 3개"를 모델링하는 축 |
| `models[].provider` | string | — | (키, 모델) 상한이 속한 프로바이더 | 선언돼 있어야 한다 |
| `models[].model` | string | — | 상한이 세는 **업스트림** 모델 이름 | 빈 값 거부 |
| `models[].max_concurrency` | int | `0` | (키, 모델)당 상한 | "모델별 7개, 그래서 모델 둘이면 14"를 모델링하는 축 |
| `principals.<id>.max_concurrent` | int | `default`에 대해 `32` | 호출자별 상한. `<id>`는 **API 키 id**이고, `default` 항목이 자기 항목 없는 모든 호출자에 적용된다 | 음수 거부. `default` 항목은 항상 존재한다 — 쓰지 않으면 `{max_concurrent: 32}`가 삽입된다 |
| `global.max_concurrency` | int | 미설정 | 프로세스 또는 클러스터 전역 상한 | 부재는 전역 상한 없음 |
| `interactive_reserve` | float | `0.3` | batch가 점유할 수 없는 **모든** 축의 비율 | `[0,1)` 이내여야 한다. `1.0` 이상은 거부 |

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
  에러를 반환한다.

엄격 FIFO와 예비도 충돌한다: 큐 head에서 막힌 batch 대기자가 예비가 보호하려는 바로 그 인터랙티브
트래픽을 head-of-line 블로킹한다. 다시 막힌 head 대기자는 시퀀스 번호를 유지한 채 그 패스 동안 주차되어
다음으로 오래된 대기자에게 차례가 간다.

---

## 9. `key_rotation`

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `strategy` | `round_robin` \| `least_used` \| `failover` \| `random` | rotation이 설정되면 `least_used`, 아니면 미설정 | 키 풀 순서를 정하려는 의도 | 그 외 거부. ⚠️ **이 빌드는 작동시키지 않는다** — 크리덴셜은 배포 내 설정 순서로 시도된다. §23.1 |
| `providers.<name>.affinity_group` | string | `""` | 풀 라벨 | — |
| `providers.<name>.stickiness.scope` | `session` \| `api_key` \| `user` \| `team` \| `tenant` \| `none` | `""` | 크리덴셜 핀이 키잉되는 범위 | 그 외 거부 |
| `providers.<name>.stickiness.on_capacity` | `wait` \| `spill` | `""` | **고정되지 않은** 요청이 선호 크리덴셜 포화 시 하는 일 | 그 외 거부. 고정된 요청에는 **절대** 적용되지 않는다 (§7.1) |
| `providers.<name>.keys[].id` | string | — | 이 키의 상한이 세는 크리덴셜 id | 빈 값·풀 내 중복 거부. *다른* 프로바이더의 크리덴셜을 가리키면 이름과 함께 거부 |
| `providers.<name>.keys[].max_concurrency` | int | `0` | `key` capacity 축 | 음수 거부; `leased`에서 `cluster.min_leasable` 미만 거부 |
| `providers.<name>.keys[].capacity_group` | string | `""` | credential-group 멤버십 | `capacity.credential_groups`에 선언돼 있어야 한다 |

---

## 10. `models`, `aliases`, `classes`

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
| `models[].strategy[]` | []string | `[prefix_sticky, lowest_cost, least_busy]` | tie-break 체인. 허용: `round_robin`, `least_busy`, `lowest_cost`, `lowest_latency`, `highest_tps`, `sticky`, `prefix_sticky`, `priority`, `weighted_random` | 그 외 거부. 설계의 `quota_urgency`는 이 빌드에서 **허용값이 아니다**(§23.1). 기본 체인은 "따뜻하게, 그다음 싸게, 그다음 한가하게" |
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
| `prefix.ttl` | duration | `1h` | 엔트리 수명. 세션 핀과 달리 prefix 엔트리는 **사용 시 갱신된다** — 여전히 쓰이는 prefix는 업스트림 캐시를 따뜻하게 유지하고 있다 | prefix가 켜져 있으면 0 이하 거부 |

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

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `on.<cause>` | []string | 설계 §7.6 표 | 원인별 체인. 원인: `rate_limit`, `quota_exhausted`, `context_window`, `content_policy`, `upstream_5xx`, `timeout`, `budget_exceeded`, `auth`. 대상: `same_group`, `same_class`, `same_class_larger` | 알 수 없는 원인·대상 거부. **`budget_exceeded`와 `auth`는 빈 체인이어야 한다** — 비어 있지 않으면 거부 |
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

### 13.1 키

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `catalog` | path | `""` | 외부 가격표, 인라인 규칙과 합쳐 하나의 카탈로그로 | **이름은 있는데 파일이 없는 것은 치명적이지 않다** — 인라인 규칙이 전체 가격표일 수 있고, 가격 없는 모델은 이미 카운터로 보인다. 형식이 잘못된 것은 치명적이다 |
| `currency` | string | `USD` | 모든 금액의 ISO 코드. 카탈로그 파일의 `currency`를 override | 빈 값 거부 |
| `rules[].id` | string | `""` | 규칙 정체성이자 최종 결정적 tie-break | 중복 id 거부 |
| `rules[].class` | `marginal_usage` \| `fixed_subscription` \| `adjustment` | `marginal_usage` | 어떤 종류의 비용인가 | 그 외 거부 — 카탈로그 전용인 `notional_rate` 포함 (§13.3) |
| `rules[].priority` | int | `0` | id보다 먼저 specificity 동률을 가른다 | 음수 거부 |
| `rules[].match.{credential,provider,model,model_prefix,deployment}` | string | `""` | specificity 사다리 | 선언되지 않은 `provider`나 `credential`은 거부. `model`과 `model_prefix`는 선언된 모델과 대조되지 **않는다** — 현재 어떤 배포도 서빙하지 않는 모델을 정당하게 가격 매길 수 있다 |
| `rules[].rates.<component>` | decimal | — | 단위당 요율. 컴포넌트: `input`, `output`, `cached_read`, `cache_write`, `reasoning`, `request`, `characters`, `seconds`, `images` | 요율 없는 `marginal_usage` 규칙 거부. ⚠️ **`images`는 검증을 통과하고 조립에서 거부된다** — 요청 타입이 이미지 수를 싣지 않아 도달 불가능한 컴포넌트이며, 조용히 0으로 가격 매기는 대신 보고된다. 이미지 엔드포인트에는 `request`를 쓸 것 |
| `rules[].period` + `rules[].amount` | string + decimal | — | `fixed_subscription`의 주기와 비용 | 그 클래스에 둘 다 필수; 금액 0 거부 |
| `rules[].percent` | decimal | — | `adjustment`의 퍼센트 | 그 클래스에 필수; 0 거부 |

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

> 레버리지 비율을 믿기 전 주의 하나. 정액 플랜에는 보통 `marginal_usage` 규칙이 아예 없으므로 amortization이
> 경과 비율로 떨어지고 분모가 사용량이 아니라 *시간*으로 안분된다. 전체 주기에 대해서는 비율이 맞지만, 주기
> 안에서는 한산한 시간이 낮은 레버리지로, 바쁜 시간이 훌륭한 레버리지로 읽힌다 — 둘 다 플랜에 대한 사실이
> 아닌데도.

`dorangctl price <model> --input N --output N`으로 미리 볼 수 있으며, 원장이 돌리는 같은 평가기를 돌린다.

---

## 14. `metering`

큐 둘. 수치 회계는 **절대 드롭되지 않는** CPU별 고정 카디널리티 카운터로 가고, 트레이스 페이로드는 상한 있는
링을 지나 내구 로컬 spool로 가며 **드롭 가능**하고 드롭은 계수된다.

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `numeric.enabled` | bool | `true` | 비용·토큰·에러 수 | ⚠️ **`false`는 거부**되며, 대신 `trace.sample_rate`를 낮추라는 메시지가 나온다. 수치 회계는 어떤 back-pressure에서도 살아남아야 하고, 두 큐 분리의 존재 이유가 그것이다 |
| `trace.store_messages` | `none` \| `hash` \| `truncated` | `truncated` | 메시지 본문 중 무엇이 `request_traces`에 도달하는가 | 그 외 거부. 요청을 다시 읽을 수 있게 해 주는 것은 `truncated`뿐이다 |
| `trace.truncate_chars` | int | `512` | 발췌 길이 | 음수 거부. 발췌는 substring으로 유지되는 대신 바이트 arena에 복사된다 — 400 KiB 본문의 512바이트 substring은 큐에 있는 동안 본문 전체를 붙든다 |
| `trace.sample_rate` | float | `1.0` | 트레이스 페이로드를 보관할 요청 비율 | `[0,1]` 이내. 엔터프라이즈 티어에서 100% 발췌는 하루 ~88 GB, 노트북 티어에서는 ~44 MB |
| `trace.daily_byte_budget` | size | `8GiB` | 트레이스 바이트의 하드 일일 상한 | 음수 거부. 희망이 아니라 강제된다 |
| `spool.dir` | path | `~/.dorang/spool` | 트레이스 큐와 스토어 사이의 내구 버퍼. 스토어 정체가 **데이터가 아니라 디스크**를 쓰게 한다 | 빈 값 거부 |
| `spool.max_bytes` | size | `2GiB` | spool 자체 상한 | 음수 거부. 도달하면 트레이스를 드롭하고 계수한다 — 프로세스가 죽을 때까지 자라는 큐는 스토어 장애를 서비스 장애로 바꾼 것이다 |
| `flush_interval` | duration | `250ms` | 카운터 병합과 spool 출하 주기 | 0 이하 거부. 크래시가 정밀도를 잃는 창이기도 하다: 쿼터 링과 롤업은 마지막 upsert + 원장으로 재구성 가능하므로 꼬리를 잃는 것은 정확성이 아니라 정밀도의 비용이고, 이 간격이 그 양이다 |

**측정된 계측 비용:** 정상 상태 +148 ns, **버퍼 가득에서 +110 ns**, 즉 200 µs warm-local p50 예산의
0.074%. 버퍼 가득이 *더 싼* 것은 분리가 설계대로 동작하는 것이다 — 실패한 링 push는 페이로드 복사를 건너뛰고
수치 경로는 동일한 일을 한다. "계측 on/off < 5%"에는 분모가 필요했다: no-op meter 대비 비율은 7.6×지만
no-op은 분기 하나 뒤에 반환하므로 그것으로 나누는 것은 실제 요청에 대해 아무것도 재지 않는다. 요구사항은
게이트웨이 오버헤드 예산의 5%다.

샘플링 제외는 의도적으로 degraded 플래그를 올리지 **않는다.** 혼동하면 샘플링된 배포에서 플래그가 영구히
참이 되고, 그것은 플래그가 없는 것과 같다.

⚠️ **degraded 상태는 이 빌드에서 관측 불가능하다.** §23.2.

---

## 15. `observability`

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `prometheus` | bool | `true` | `/metrics`를 게이팅하려는 의도 | ⚠️ **절대 읽히지 않는다.** `/metrics`는 무조건, 인증 없이 서빙된다. `false`는 아무것도 바꾸지 않는다. §23.1 |
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

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `lua.enabled` | bool | `false` | 샌드박스 훅 활성화. 꺼져 있으면 엔진이 typed nil이고 핫패스는 nil 검사 하나 비용이다 | — |
| `lua.dir` | path | `/etc/dorang/lua` | 훅 프로그램 위치 | 활성화 시 빈 값 거부 |
| `lua.hooks[]` | []string | `[]` | 허용: `on_request`, `on_route`, `on_response`, `on_email` | 그 외 거부 |
| `lua.limits.{instructions,memory_mb,timeout}` | int/int/duration | `5000000` / `32` / `200ms` | 훅별 상한 | 음수 거부. **활성화된 훅에서 셋 중 하나라도 0이면 거부** — 핫패스의 무제한 훅은 훅이 아니라 나쁜 프로그램을 기다리는 장애다 |

의미론: 상한 초과는 훅을 건너뛰고 경고한다(**fail-open**). 예외는 `on_request`의 명시적 거부이며 그것은
존중된다(**fail-closed**). 훅 안의 panic은 격리된다. 어떤 훅도 비밀값을 볼 수 없다 — 훅에 건네지는 view는
나중에 걸러내는 것이 아니라 애초에 키 자료 없이 구성된다.

> ⚠️ **절 이름이 `lua`인데 Lua 인터프리터가 없다.** 그것은 결정이고, 설정 키가 그렇게 말해 주지 않으므로
> 분명히 적어 둘 가치가 있다.
>
> 범용 VM을 임베드하면 프로젝트의 첫 비-저장소·비-YAML 의존성이 생기고, 더 결정적으로 **약속된 세 상한 중
> 둘이 쉬워지는 게 아니라 어려워진다**: 진지한 순수 Go 선택지는 협조적 취소는 제공하지만 state별 명령어
> 카운터도 state별 메모리 회계도 없어서, "명령어와 메모리 상한"은 그 위에 다시 구현하거나 조용히 철회해야
> 한다. 조용한 철회야말로 이 문서 전체가 막으려는 결과다.
>
> 대신 도는 것: 루프도 재귀도 함수 호출도 I/O도 문자열 생성도 없는 **total 정책 언어**(`*.policy` 파일).
> 프로그램이 구성상 종료하고 무제한으로 할당할 수 없다 — 상한은 여전히 강제되지만, 확장과 게이트웨이
> 사이에 서 있는 유일한 것이 아니라 심층 방어로서다. 정책 언어가 표현할 수 없는 것을 위해 컴파일된 Go
> **`Native`** 훅이 있고, 같은 watchdog·panic 격리·fail-open 규칙·비밀값 없는 view를 물려받는다
> (`Native`에는 벽시계와 panic 격리만 적용되고, 메모리 상한은 정책 프로그램에 대해 의미를 갖는다).
>
> **`extensions.lua.dir` 아래의 `.lua` 파일은 조용히 무시되는 파일이 아니라 로드 에러다.** 절대 돌지 않는
> Lua를 받아들이는 설정 표면은 거부하는 표면보다 나쁘다.
>
> 훅 지점이 의도적으로 갖지 **않는** 능력 하나: `on_route`는 선택된 배포를 보고 거부할 수 있지만 다른 것을
> 요구할 수는 없다. 그것은 훅이 아니라 fail-back 기계의 성질이다.

---

## 17. `passthrough`

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

## 18. `shadow`

마이그레이션 중 라이브 트래픽의 표본을 참조 게이트웨이와 비교한다. 전체 절차는
[MIGRATION.ko.md](MIGRATION.ko.md) §4.

| 키 | 타입 | 기본값 | 하는 일 | 틀리면 |
|---|---|---|---|---|
| `mode` | `off` \| `mirror` \| `compare` | `off` | `mirror`는 보내고 기록, `compare`는 보내고 기록하고 diff | 그 외 거부. `"off"`를 따옴표로 감쌀 것 — 맨 `off`는 YAML boolean false다 |
| `reference.url` | string | `""` | 비교 대상 게이트웨이 | mode가 `off`가 아니면 필수. `http`/`https`여야 하고, host가 있어야 하며, **쿼리나 프래그먼트가 없어야 한다** — 둘 다 요청 경로와 결합되면 살아남지 못한다 |
| `reference.api_key_env` | string | `""` | **참조 게이트웨이 자신의** 크리덴셜 변수 이름. 클라이언트 크리덴셜은 절대 그리로 전달되지 않는다 | 이 파일에서 유일하게 `key_env`/`key_file`/`key_ref` 삼종이 아니라 `api_key_env`로 비밀 참조를 쓰는 곳이다. **비밀값을 파일이나 볼트에 두는 배포는 shadow 참조 크리덴셜을 아예 표현할 수 없다** |
| `reference.timeout` | duration | `60s` | 참조 호출 하나의 상한 | 음수 거부. `server.request_timeout`보다 의도적으로 짧다: 자기가 복사한 요청보다 오래 사는 shadow 호출은 아무도 읽지 않을 비교를 위해 워커를 붙들고 있는 것이다 |
| `sample_rate` | float | **`0`** | shadow할 적격 요청 비율 | `[0,1]` 이내. ⚠️ **기본값이 없다.** 이것을 설정하지 않고 `mode`를 켜면 아무것도 shadow하지 않고, 빈 리포트를 쓰고, 게이트 판정 `no_data`를 낸다 — 그리고 빈 리포트는 컷오버 결정이 찾는 바로 그것이다 |
| `compare.structural` | bool | `true` | 상태, 필드 경로 집합, 경로별 타입, 헤더 키, 에러 봉투를 비교 | `mode: compare` + structural off는 "대신 `mode: mirror`를 쓰라"며 거부 |
| `compare.semantic` | bool | `false` | — | ⚠️ **true는 거부된다.** 설계가 노브를 명명하고 비교를 명세하지 않는다. 조용히 받아들이면 의미 비교를 요청한 오퍼레이터가 구조 비교와 빈 리포트를 받고 그것을 검사한 적 없는 무언가의 증거로 읽는다 — 컷오버 게이트가 가질 수 없는 유일한 실패 모드다 |
| `compare.ignore_fields[]` | []string | `[]` | 내장 집합(id, 타임스탬프, `system_fingerprint`, 출력 텍스트, 토큰 수) 위에 추가로 제외할 필드 경로. 맨 이름은 임의 깊이 매칭, `$.` 시작은 정확 매칭, 후행 `*`는 프리픽스 매칭 | 빈 항목 거부. 무시하는 모든 필드는 clean 리포트가 아무 말도 하지 않는 필드다 |
| `max_cost_usd_per_day` | decimal | — | 참조 게이트웨이 지출의 일일 상한 | ⚠️ mode가 `off`가 아니면 **필수**. 두 모드 모두 표본 요청을 두 번 보내므로 두 배 비싸다 |
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

## 19. `notifications`

이메일 시스템이 아니라 **싱크를 갈아 끼울 수 있는 이벤트 버스**다. `email` 아래는 전달이고, 그 옆의 모든
것은 파이프라인이다 — 파이프라인이 존재하는 이유는 알림이 지연 쓰기이고 §9.6이 지연 쓰기에 상한이 있고,
드롭될 때 보이고, 절대 요청 경로에 있지 않기를 요구하기 때문이다.

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
| `email.smtp.key_env` / `key_file` / `key_ref` | secret ref | — | SMTP 비밀번호, 평범한 비밀 참조로 (§0.3) | development 밖의 인라인 리터럴은 다른 곳과 마찬가지로 거부 |
| `email.smtp.starttls` | bool | `false` | 연결을 업그레이드 | ⚠️ **username을 설정하고 `starttls: false`면 거부된다.** Go의 SMTP 클라이언트는 loopback이 아닌 서버에 암호화되지 않은 연결로 `PLAIN`을 보내지 않으므로, 그러지 않으면 전달 시점에 실패한다; 로드에서 말해 주는 쪽이 더 싼 발견이다 |
| `email.smtp.tls_skip_verify` | bool | `false` | 검증 불가한 인증서 수용 | 사설 릴레이를 위한 의도적 다운그레이드 |
| `email.smtp.timeout` | duration | `10s` | 전달별 타임아웃 | 음수 거부 |
| `email.smtp.helo` | string | `""` | dorang이 자신을 알리는 이름 | — |
| `email.http.url` | string | `""` | webhook 엔드포인트 | `http`에 필수. host가 있는 `http`/`https`여야 한다. **여기서는 쿼리 문자열이 괜찮다** — shadow 참조와 달리 URL이 요청 경로와 결합되지 않고 통째로 쓰이며, 호스팅 webhook은 흔히 토큰을 쿼리에 싣는다 |
| `email.http.key_env` / `key_file` / `key_ref` | secret ref | — | 서명 비밀값 | ⚠️ **선택이 아니라 필수.** 전달은 본문에 대한 HMAC-SHA256으로 서명되고, 비밀값이 없는 수신자는 진짜 전달과 위조를 구별할 수 없다. 페이로드가 예산과 쿼터 상태를 싣기 때문에, 서명 없는 webhook은 URL이 닿는 순간 정보 유출이다 |
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

## 20. `priority_mapping`

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

비대칭은 의도적이다: **오퍼레이터는 긴급성을 부여할 수 있고, 호출자는 주장할 수 없다.** 설계는 그 부여를
`capacity.principals.<id>.client_priority` + `range`로 표현한다. ⚠️ 그것은 **이 빌드 스키마에 없다** —
무시 동작은 구현돼 있고 부여는 아니다. §23.1.

힌트 드롭은 조용하지 않고 `x-dorang-dropped-params`에 보고된다.

---

## 21. 참조 무결성

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

## 22. 환경 변수

| 변수 | 읽는 곳 | 이름 설정 위치 | 비고 |
|---|---|---|---|
| `DORANG_CONFIG` | `dorang`, `dorangctl` | — | `--config`의 기본값. 설정하면 경로가 **명시적**이 되므로 없는 파일이 에러가 된다 |
| `DORANG_MASTER_KEY` | 서버 | `server.master_key_env` | out-of-band 관리 크리덴셜 |
| `DORANG_KEY_PEPPER` | 서버, `dorangctl key` | `server.key_pepper_env` | HMAC pepper. 미설정이면 DB 옆에 생성 — §2 |
| `DORANG_DATABASE_URL` | 서버 | `storage.postgres.url_env` | `driver: postgres`일 때만 |
| `DORANG_REDIS_URL` | 서버 | `cluster.redis_url_env` | `capacity_mode: shared-redis`일 때만 |
| `DORANG_CATALOG_PATH` | 서버, `dorangctl` | — | PATH 구분 모델 카탈로그 레이어, **마지막**에 적용. 없는 레이어는 skip이 아니라 **에러**: 조용히 떨어진 레이어야말로 카탈로그가 답하려는 질문 그 자체다 |

`dorangctl key create`와 `dorangctl migrate`는 서버와 같은 방식으로 pepper를 해결한다. 다른 pepper로 키를
발급하는 CLI는 서버가 검증할 수 없는 키를 발급하게 된다.

⚠️ `DORANG_STATE_DIR`은 컨테이너 이미지가 설정하고 **아무것도 읽지 않는다.** §23.1.

---

## 23. 스키마가 받지만 이 빌드가 작동시키지 않는 키

이 절의 모든 것은 검증되고 로드되며 아무 효과가 없다. 설정했다고 믿는데 아무 일도 하지 않는 상한은 상한이
없는 것보다 나쁘므로 이름으로 열거한다.

### 23.1 받아들여지고 무력한 것

| 키 | 상태 |
|---|---|
| 모든 그룹과 `global`의 `capacity.*.rpm`, `.tpm`, `.max_queue` | 부호만 검증; broker는 세는 게이지만 구현한다. 토큰 버킷도 큐 상한도 존재하지 않는다 |
| `capacity.principals.<id>.rpm`, `.tpm`, `.max_queue`, `.max_queue_wait` | 동일. `max_queue_wait`는 모든 principal에 `30s`로 기본값이 채워지고 소비자가 없다 |
| `metric: max_concurrent` 또는 `max_queue`인 `models[].deployments[].limits[]` | `rpm`과 `tpm`만 소비되며, 크리덴셜별 쿼터로 (§10.2) |
| `key_rotation.strategy` | 네 이름에 대해 검증; 크리덴셜은 어쨌든 설정 순서로 시도된다 |
| `observability.prometheus` | 절대 읽히지 않음. `/metrics`는 무조건이고 **인증이 없다** |
| `observability.otlp_endpoint` | exporter가 연결돼 있지 않다 |
| `DORANG_STATE_DIR` | 컨테이너 이미지가 선언; 읽는 코드 없음. state 경로는 `storage.sqlite.path`와 `metering.spool.dir`에서 온다 |

### 23.2 설계돼 있고 스키마에 아예 없는 것

| 설계 절 | 없는 것 |
|---|---|
| §6.1 `quotas:` — `cost_usd`나 `tokens_total`에 대한 롤링 `5h`/`daily`/`weekly`/`monthly` 윈도우와 `on_exhaust` | **최상위 `quotas:` 블록이 없다.** 이 빌드가 만드는 유일한 쿼터 규칙은 `deployments[].limits[].rpm`/`.tpm`에서 파생된 롤링-분 요청·토큰 카운터다 |
| §6.4 `budget:` — `period`/`limit_usd`/`on_exceed` 블록 | **최상위 `budget:` 블록이 없다.** 예산은 API 키별이며 `dorangctl key create --budget-usd`로 설정하고 내구 원장으로 강제된다 |
| §7.5a `quota_urgency` | 허용되는 전략 이름이 아니다. 만료 임박 쿼터 comparator는 설계됐고 만들어지지 않았다 |
| §10.5 `capacity.principals.<id>.client_priority` + `range` | `PrincipalLimits`에 없다. 기본값(`ignore`)은 구현됐고 부여는 아니다 |
| §7.4a2 `stickiness.pin_on_state` | 스키마에 없고 — 그것이 옳다: 핀은 설정이 아니라 요청에서 추론된다 (§7.1) |
| §12.1 `metering_degraded` | meter는 다섯 사유와 히스테리시스로 degradation을 추적하고 **아무것도 그것을 읽지 않는다**: 메트릭도, health 필드도, admin 필드도 없다. 설계의 "절대 조용하지 않다"는 이 빌드에서 참이 아니다. (`notifications`는 별도로 보고되는 자체 degraded 상태를 갖는다 — 다른 신호다) |
| §11.5 "Lua 훅" | 훅 지점, 상한, fail-open/fail-closed 규칙, 비밀값 없는 view는 구축돼 있다; **Lua 인터프리터는 없고** `.lua` 파일은 로드 에러다. §16이 이유와 대신 도는 것을 설명한다 |
| §8.5 `pricing.rules[]`의 `notional_rate` | 인라인 규칙은 세 클래스를 받는다. notional 규칙은 외부 카탈로그 파일에 있어야 한다 (§13.3) |

### 23.3 `--check`가 잡지 못하는 것

- 린트하는 기계에는 설정돼 있고 서빙하는 기계에는 없는(또는 그 반대인) `key_env` 변수.
- 내용이 잘못된 `storage.postgres.url_env`.
- 조립에서 실패하는 `pricing.rules[].rates.images`.
- 프로바이더에 `base_url`이 없어 조용히 삭제되는 `passthrough.routes[]` 항목.
- 업스트림이 존재하는지, 응답하는지, `kind`가 주장하는 프로토콜을 말하는지에 대한 그 무엇도.

---

## 24. 세 가지 실제 설정

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

전체 예시 YAML은 기준 문서 [CONFIG.md](CONFIG.md) §24에 있다. 각 티어에서 실제로 중요한 지점만 옮긴다.

### 24.1 노트북 — 의존성 0

프로세스 하나, 파일 하나, 설치할 것 없음. 바이너리가 정적(SQLite 드라이버가 순수 Go)이라 `scratch`에서도
돈다.

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

- **`DORANG_KEY_PEPPER`를 명시적으로 설정할 것.** 노드 둘에 생성된 pepper면 각자 자기 것을 만들고 한쪽이
  발급한 키를 다른 쪽이 검증할 수 없다.
- ~50 req/s에서 `sample_rate: 0.2`는 발췌를 하루 ~2.2 GB 대신 ~440 MB로 유지한다.
- plan-a 숫자가 축이 존재하는 이유의 형태다: 모델별 7 *그리고* 계정별 7이면 모델 둘이 한 키에서 동시
  14에 이르는 동안 계정 상한은 provider-group 수준에서 여전히 지켜진다.
- `cloud-a`의 `usage_probe`가 dorang을 거치지 않은 트래픽까지 쿼터에 반영시킨다(§6.1).
- `shutdown_grace`를 p99보다 길게 둘 것. 롤링 재시작이 조용해진다.

### 24.3 엔터프라이즈 — 의존성 2

N 노드, 정확한 공유 capacity, 파티션 원장, 강한 샘플링.

- **`node_id`는 노드별로 구별되고 안정적이어야 한다.** 빈 값은 *프로세스*마다 파생하므로 재시작한 노드가
  자기 리스를 회수하지 못하고 만료를 기다린다. 오케스트레이터에서 pod 이름 등으로 주입할 것.
- `capacity_mode: shared-redis`는 핫패스에 왕복 하나를 물리며, 그 프로파일의 p99 목표는 5 ms다. 정확한
  회계의 값이고, `leased`는 그것을 공표된 초과 `block × (nodes − 1)`과 맞바꾼다.
- `min_leasable: 16`은 `shared-redis`에서 무력하고 `leased`로 바꾸는 순간 하중을 받는다 — 그 시점에
  파일의 16 미만 `max_concurrency`가 전부 거부된다.
- ~2 k req/s에서 `store_messages: hash` + `sample_rate: 0.01`은 전체 발췌의 하루 ~88 GB 대신 ~880 MB를
  보관한다.
- `key_ref`는 **이 빌드에서 기록되고 해결되지 않는다**: 외부 resolver가 생기기 전까지 그 크리덴셜은
  로드되고 쓸 수 있는 비밀값이 없다. 오늘은 `key_env`나 `key_file`을 쓸 것.
- vLLM `metrics` 스크레이프는 `--disable-log-stats`가 **없을** 때만 유용하다. 그것이 있으면 `/metrics`가
  시리즈 0개로 200을 반환하고 `least_busy`가 모든 백엔드를 유휴로 읽는다([VLLM.ko.md](VLLM.ko.md) §3.1).

---

## 함께 보기

- [OPERATIONS.ko.md](OPERATIONS.ko.md) — 설치·운영·메트릭 읽기, 그리고 문제가 생겼을 때.
- [MIGRATION.ko.md](MIGRATION.ko.md) — 기존 게이트웨이의 설정·크리덴셜 임포트와 shadow 컷오버.
- [DESIGN.ko.md](DESIGN.ko.md) §4 — 스키마의 근거.
- [VLLM.ko.md](VLLM.ko.md) §5, [SGLANG.ko.md](SGLANG.ko.md) §8 — 위의 모든 것이 말한 대로 의미하기 전에
  self-hosted 백엔드에 필요한 플래그.
