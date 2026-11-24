# CLAUDE.md

이 저장소에서 작업할 때 참고하는 안내서다. 사용법과 설계 배경은 [README.md](README.md)에 있으니 여기에는 반복하지 않는다.

## 프로젝트 한 줄 요약

ConfigMap이 바뀌면 Pod를 교체하지 않고 시그널(기본 `SIGHUP`)로 앱의 설정을 다시 읽게 하는 Kubernetes 오퍼레이터. kubebuilder로 만든 Go 프로젝트다.

- Go 모듈: `github.com/nagisatosanae/config-reload-operator` (Go 1.26, controller-runtime v0.25)
- API: `reload.nagisatosanae.github.io/v1alpha1`, kind `ConfigReloadPolicy` (short name `crp`)
- 범위: Deployment, 같은 네임스페이스, 디렉터리로 마운트한 ConfigMap만 지원한다.

## 구조

두 개의 바이너리가 있고, 서로 직접 통신하지 않는다. 둘을 잇는 것은 컨테이너의 `WATCH_*` 환경 변수뿐이다.

| 경로 | 내용 |
|---|---|
| `cmd/watcher/` | watcher 바이너리. 앱 이미지 안에서 PID 1로 실행되고 앱을 자식 프로세스로 띄운다. 설정 디렉터리를 감시하고, 내용 해시 비교 → debounce → 검증 명령 → 시그널 순서로 동작한다. Kubernetes API를 호출하지 않는다. |
| `cmd/manager/` | controller 바이너리의 진입점. |
| `internal/controller/` | `ConfigReloadPolicy` reconciler, Deployment 웹훅(`deployment_webhook.go`)과 테스트. |
| `api/v1alpha1/` | CRD 타입. 기본값과 검증은 kubebuilder 마커로 정의한다. |
| `charts/config-reload-operator/` | Helm 차트. 공식 설치 경로다. |
| `config/` | controller-gen이 만든 CRD와 RBAC, 샘플 정책. |
| `demo/nginx/` | 데모 이미지(NGINX + watcher)와 매니페스트. |
| `docs/` | 추가 문서. 이미지와 릴리스 방법은 [docs/images.md](docs/images.md). |
| `notes/` | 개인 학습 노트. gitignore 대상이다. |

### 꼭 알아야 할 동작

- **환경 변수 계약**: `WATCH_DIR`, `WATCH_SIGNAL`, `WATCH_DEBOUNCE_SECONDS`, `WATCH_VALIDATE_COMMAND`(JSON 문자열 배열). 이름이 `cmd/watcher/main.go`와 `internal/controller/configreloadpolicy_controller.go` 양쪽에 상수로 따로 정의되어 있다. 하나를 바꾸면 다른 쪽도 같이 바꾼다.
- **`WATCH_DIR`가 비어 있으면** watcher는 앱만 실행한다. 정책이 없어도 이미지가 안전하게 동작해야 한다.
- **멱등성**: controller는 값이 실제로 다를 때만 Deployment와 status를 쓴다. 반복 reconcile이 rollout을 일으키면 버그다.
- **소유 범위**: controller는 `ownedEnv`에 있는 네 개의 env만 건드린다. 사용자가 넣은 다른 env는 그대로 둔다.
- **Deployment 패치**는 `client.StrategicMergeFrom`을 쓴다. `client.MergeFrom`은 containers 목록 전체를 덮어쓰므로 쓰지 않는다.
- **삭제**: finalizer `reload.nagisatosanae.github.io/revert`가 env를 걷어낸 뒤 정책을 놓아준다. 정책을 지워도 사용자의 Deployment나 ConfigMap은 절대 지우지 않는다.
- **준비되지 않은 정책**은 `Ready=False`와 reason을 남기고 30초 뒤 다시 확인한다. 이때 Deployment는 건드리지 않는다.
- **controller는 reload 경로에 없다.** controller가 내려가 있어도 reload는 계속 동작해야 한다.
- **웹훅은 지름길이다.** `DeploymentInjector`는 애노테이션 `reload.nagisatosanae.github.io/inject: "true"`가 있는 Deployment가 만들어지거나 수정될 때, reconciler와 같은 함수(`findWatchDir`, `watcherEnv`, `syncEnv`)로 env를 넣는다. 정책이 Deployment보다 먼저 있을 때만 효과가 있다. 그 밖의 경우와 웹훅이 놓친 것은 reconciler가 맞춘다.
- **웹훅은 요청을 거부하지 않는다.** `Default`는 항상 `nil`을 돌려준다. 오류를 돌려주면 `failurePolicy: Ignore`여도 사용자의 Deployment가 거부된다.
- **웹훅은 삭제 중인 정책을 건너뛴다.** finalizer가 env를 걷어내는 patch도 웹훅을 지나가므로, 건너뛰지 않으면 걷어낸 env를 다시 넣게 된다.
- **웹훅의 이름들이 두 곳에 있다**: 애노테이션, 경로(`/mutate-apps-v1-deployment`), 필터 식이 `internal/controller/deployment_webhook.go`와 `charts/config-reload-operator/templates/webhook.yaml`에 따로 적혀 있다. 하나를 바꾸면 다른 쪽도 같이 바꾼다. `TestChartMatchesTheWebhook`이 어긋남을 잡는다.
- **웹훅은 인증서가 있을 때만 켜진다.** manager는 `--webhook-cert-path`가 있을 때만 웹훅을 등록한다. 차트는 `webhook.enabled`일 때 그 인자와 Helm이 만든 인증서 Secret을 넣는다. cert-manager는 쓰지 않는다.

## 명령어

`make`와 watcher 테스트는 Linux가 필요하다. Windows에서는 WSL2 안에서 실행한다. watcher는 Linux 시스템 콜을 쓰기 때문에 Windows에서는 테스트가 돌지 않는다.

```sh
go test -short ./...                  # 빠른 테스트: 순수 Go 테스트만 (envtest 건너뜀)
make test                             # 전체: manifests, generate, fmt, vet 후 envtest 포함
go test ./cmd/watcher/ -run TestName  # watcher 테스트 하나만
make lint                             # golangci-lint
make lint-fix                         # 자동 수정까지

make manifests                        # CRD와 RBAC 다시 생성, 차트의 crds/로 복사
make generate                         # zz_generated.deepcopy.go 다시 생성

./build.sh                            # watcher를 static linux 바이너리로 bin/watcher에 빌드
docker build -f demo/nginx/Dockerfile -t demo-nginx:dev .
make docker-build IMG=manager:dev     # manager 이미지
```

- `api/v1alpha1/*_types.go`나 `+kubebuilder` 마커를 고치면 `make manifests generate`를 실행하고 생성된 파일을 같이 커밋 대상에 넣는다.
- 생성 파일은 손으로 고치지 않는다: `zz_generated.deepcopy.go`, `config/crd/bases/`, `config/rbac/role.yaml`, `charts/config-reload-operator/crds/`.
- watcher와 demo 이미지는 Docker 밖에서 빌드한 `bin/watcher`를 복사한다. 이미지를 만들기 전에 `./build.sh`를 먼저 실행한다.
- e2e 테스트는 없다. 전체 흐름은 README의 quick start로 직접 확인한다.
- `kubebuilder create ...`는 이 저장소에서 끝까지 돌지 않는다. `cmd/main.go`와 `test/e2e/`를 찾는데 둘 다 없다. 새 조각은 저장소의 구조에 맞춰 손으로 쓴다. 웹훅 설정도 `+kubebuilder:webhook` 마커가 아니라 차트 템플릿에 직접 쓴다.

## 테스트

- watcher: 표준 `testing` 패키지. kubelet의 `..data` 심볼릭 링크 교체를 임시 디렉터리에서 흉내 낸다.
- controller: Ginkgo + Gomega + envtest(실제 API 서버와 etcd). `-short`를 주면 envtest 스위트는 건너뛴다. 웹훅도 같은 스위트에서 envtest API 서버가 실제로 부른다.
- 동작을 바꾸면 테스트도 같이 바꾼다. README의 테스트 개수 표와 어긋나면 표도 맞춘다.

## 브랜치와 릴리스

- 기본 브랜치는 `main`, 평소 작업은 `develop`에서 한다.
- GitHub Actions: push와 PR마다 `test.yml`, `lint.yml`이 돈다. `v*` 태그를 push하면 `images.yml`이 `watcher`, `demo-nginx`, `manager` 이미지를 ghcr.io에 올린다.
- 릴리스할 때 버전을 맞춰야 하는 곳: `charts/config-reload-operator/Chart.yaml`(`version`, `appVersion`), README와 `docs/images.md`의 watcher 이미지 태그.

## 작업 규칙

- **커밋과 push는 사용자가 직접 한다.** `git commit`, `git push`, `git tag`를 실행하지 않는다.
- **저장소에 들어가는 글은 영어로 쓴다.** 코드 주석, 커밋 메시지, README, docs 모두 짧고 쉬운 문장의 영어다. 이 파일만 예외로 한글이다.
- **`notes/`의 내용은 커밋되는 파일에 옮기지 않는다.**
- **README는 요청받은 부분만 고친다.** 섹션이나 목록을 임의로 늘리지 않는다.
- 고정된 이름을 바꾸지 않는다: 저장소와 차트 `config-reload-operator`, kind `ConfigReloadPolicy`, 바이너리 `manager`와 `watcher`.
- v1 범위 밖의 것(StatefulSet, DaemonSet, Secret, HTTP reload, 다른 네임스페이스 참조)은 요청이 없으면 만들지 않는다.
