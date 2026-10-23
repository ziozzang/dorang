# Experiential에서 공개 자료와 보관 금지 요청을 구분합니다

공개 자료용과 보관 금지용 별칭을 명시적으로 선택합니다. 기존 모델 그룹이나
자동 대체 경로에는 추가하지 않습니다. 2026-10-23 Pro 가입 완료 지시에 따라
일반 Flash 두 별칭을 활성으로 준비합니다. 추가 지시에 따라 uncensored 공개 별칭도 활성으로 준비하고, uncensored ZDR는 별도 공급자 키(BYOK) 연결 전 비활성입니다.

| 별칭 | 공급자 설정 | 강제 조건 | 사용 범위 |
|---|---|---|---|
| `experiential/deepseek-v4.1-flash-public` | `experiential` | `provider.require_no_training: true` | **ZDR 아님·공개 자료 전용**. 학습 미사용은 보관 금지가 아닙니다. |
| `experiential/deepseek-v4.1-flash-zdr` | `experiential-zdr` | `provider.zdr: true`, `provider.require_no_training: true` | Pro 필요. 성공 응답의 `x-gateway-zdr`가 정확히 `true`가 아니면 답을 전달하지 않습니다. |

| `experiential/deepseek-v4.1-flash-uncensored-public` | `experiential` | `provider.require_no_training: true` | **ZDR 아님·공개 자료 전용**. 일반 공개 별칭과 처리량 예산을 공유합니다. |
| `experiential/deepseek-v4.1-flash-uncensored-zdr` | `experiential-zdr` | `provider.zdr: true` | **비활성**. 현재 BYOK 필요로 ZDR 호출이 거절됩니다. |

uncensored는 BYOK 연결 후에도 보존 금지 가능 여부를 다시 확인해야 합니다.
ZDR 별칭을 쓰려면 먼저 공급자의 모델 접근·보존 정책을 확인하고,
`enabled: true`로 바꾼 후 성공 헤더가 true인지 검증합니다. 단순 가입이나
비활성 해제만으로 보관 금지가 보장되지 않습니다.

ZDR은 입력·출력 내용을 공급자가 보관하지 않도록 요구하는 정책입니다.
공급자의 보존 정책 판정 헤더이며 제3자 서명이나 계약 보증은 아닙니다.
호출자가 다른 `provider` 객체를 보내도 설정의 전체 객체로 대체합니다.
ZDR 오류는 공개 별칭으로 자동 전환하지 않습니다. 두 그룹은 다른 공급자
설정이지만 같은 키 파일과 동시 처리 묶음을 사용합니다.

## 한도를 바꿀 때

`deploy/experiential.yaml`은 비밀값 없는 설정 조각입니다. 운영 파일은
`/docker/dorang/config/dorang-direct.yaml`입니다.

- `providers[].max_concurrency: 4`와 새 provider/credential 묶음의
  `max_concurrency: 4`: 두 별칭을 합쳐 **노드별 동시 4건**입니다.
  직접 진단 포트나 기존 :4000에도 요청하면 노드 수만큼 늘 수 있습니다.
- 계정 전체 시작 예산은 **분당 60건(RPM), 분당 60,000토큰(TPM)**으로
  두 별칭에 각각 30건·30,000토큰을 분배했습니다. 한 별칭의 남은 몫을
  다른 별칭이 빌리지 않습니다. 각 deployment의 `limits` 값으로 바꿉니다.
  현재 shared-pg 모드에서 각각의 credential 한도를 공유합니다.
- TPS는 보통 초당 토큰 수를 뜻합니다. 이번 요구의 분당 토큰 상한은 TPM으로
  구현했습니다. 별도 초당 토큰 제한은 없습니다.
- 이 값은 운영자가 정한 상한이며 공급자가 보장한 무료 계정 한도가 아닙니다.
  공급자는 별도 조직별 분·시간·일 한도를 적용할 수 있습니다.
- 429의 대기·계정 제외·연결 재시도는 기존 공급자의 공통 처리와 같습니다.
  응답을 받기 시작한 요청을 다시 실행하지 않으며, 별칭 간 대체는 없습니다.
- `usage_probe: {enabled: true, fetcher: experiential, interval: 5m}`:
  같은 추론 키로 `/api/v1/credits`의 적립액과 사용액을 읽습니다.
  USD 잔액은 `total_credits - total_usage`입니다. 조직 전체 금액이며
  무료 모델 권한·분당 잔량을 나타내지 않습니다.
- `catalog_sync`는 설정하지 않습니다. 공개 모델 목록 전체를 자동으로 호출 경로에
  추가하지 않고, 검토한 명시한 별칭의 카탈로그를 별도 파일로 유지합니다.

설정된 로컬 토큰 예산은 큰 입력 요청을 거절할 수 있습니다. 모델 자체의
문맥 길이와 운영자가 허용하는 처리량은 서로 다른 값입니다.

## 가입 상태가 바뀔 때

Pro 미가입·해지 상태에서 ZDR 요청은 공급자가 `403 needs_subscription`으로
거절할 수 있습니다. 이때 ZDR 조건을 삭제하지 말고 ZDR 모델 deployment의
`enabled`를 `false`로 두십시오. 비활성 모델은 503으로 거절합니다.
Pro 가입 후 `enabled: true`로 바꾸고 짧은 공개 프롬프트로 HTTP 200 및
`x-gateway-zdr: true`를 확인합니다. 이 판정은 실행 중에도 매 응답에 적용됩니다.
보관 금지 확인이 없으면 502 `zdr_confirmation_missing`으로 거절합니다.
구독 결제나 조직 전체 데이터 정책은 이 배포에서 변경하지 않습니다.

## 기술 메모

- 기본 주소: `https://api.experientiallabs.ai/v1`.
- 비밀값: `~/env`의 `EXPERIENTIALLABS_API_KEY`를 메모리로만 읽고,
  stdin으로 기존 UID/GID 65532, 0600 보관 방식에 맞춰 설치합니다.
  설정에는 `/etc/dorang/secrets/experiential-key` 경로만 남깁니다.
- 실행 버전에는 `params.set` 스키마만 있고 소비 코드가 없어 이번 패치에서
  연결했습니다. 최종 JSON 변환 뒤 강제로 주입하고 비 JSON 요청은 거절합니다.
  `params.default`는 이번 변경 범위가 아닙니다.
- 기존 공급자에 `params.set` 값이 없으므로 요청 바이트는 그대로 유지됩니다.
- 카탈로그 조각: `deploy/experiential-catalog.yaml`. 공급자 제공 기능·가격을
  기록하며 도구 호출 등 모든 기능을 실제 검증했다는 뜻은 아닙니다.
- 가격표는 입력 100만 토큰당 0.30 USD, 출력 1.20 USD, 캐시 입력 0.006 USD입니다.
  무료 모델이라는 뜻이 아닙니다. 응답의 `usage.cost`를 실제 보고 금액으로
  별도 읽으며 구독·홍보 혜택으로 0일 수도 있습니다.
- 먼저 기존 설정 상태에서 새 이미지를 공식 `rollout.py deploy`로 양쪽 색상에
  순차 적용하고 `deploy-legacy`로 :4000에도 검증·인계합니다. 모든 노드가
  강제 조건 주입을 지원한 뒤 공유 설정을 반영합니다. 직접 컨테이너 재생성,
  데이터베이스 복원, 기존 경로 변경은 하지 않습니다.
- 영수증: `/data/jobs/.state/oneshot-opus/dorang-el/`.

공식 근거(2026-10-23 확인):
[데이터 정책](https://platform.experientiallabs.ai/docs/data-controls),
[잔액·정산 API](https://platform.experientiallabs.ai/docs/cost-api),
[오류와 한도](https://platform.experientiallabs.ai/docs/errors).
