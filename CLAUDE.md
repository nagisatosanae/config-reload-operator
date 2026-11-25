# CLAUDE.md

이 저장소에서 작업할 때 참고하는 안내서다. 사용법은 [README.md](README.md)에, 설계 이유는 [docs/design.md](docs/design.md)에 있으니 여기에는 반복하지 않는다.

## 프로젝트 한 줄 요약

ConfigMap이 바뀌면 Pod를 교체하지 않고 시그널(기본 `SIGHUP`)로 앱의 설정을 다시 읽게 하는 Kubernetes 오퍼레이터. kubebuilder로 만든 Go 프로젝트다.

- Go 모듈: `github.com/nagisatosanae/config-reload-operator` (Go 1.26, controller-runtime v0.25)
- API: `reload.nagisatosanae.github.io/v1alpha1`, kind `ConfigReloadPolicy` (short name `crp`)
- 범위: Deployment, 같은 네임스페이스, container마다 ConfigMap 디렉터리 하나.

## 원칙

reload는 부가 기능이다. 온전하게 동작해야 하지만, 이 기능 때문에 앱의 서비스가 장애로 이어지면 안 된다. 판단이 갈리면 앱의 안전이 먼저다. 아래의 동작은 모두 이 원칙에서 나왔다.

## 구조

바이너리는 `watcher`와 `manager` 두 개다. `manager`는 웹훅과 controller 두 역할을 하고, 차트는 역할마다 Deployment를 따로 띄운다. 세 부분은 서로 직접 통신하지 않는다. 웹훅과 controller가 공통으로 보는 것은 정책(CRD)뿐이고, 웹훅과 watcher를 잇는 것은 container의 `CONFIG_RELOAD_*` 환경 변수뿐이다.

| 경로 | 내용 |
|---|---|
| `cmd/watcher/` | watcher 바이너리와 시작 스크립트 `watcher-entrypoint.sh`. 스크립트가 watcher를 백그라운드로 띄우고 앱으로 바뀐다. watcher는 설정 디렉터리를 감시하고, 내용 해시 비교 → debounce → 검증 명령 → 시그널 순서로 동작한다. Kubernetes API를 호출하지 않는다. |
| `cmd/manager/` | manager 바이너리의 진입점. 실행 인자로 역할을 고른다. |
| `internal/controller/` | Pod 웹훅(`pod_webhook.go`), Deployment 검사(`deployment_check.go`), `ConfigReloadPolicy` reconciler와 테스트. |
| `api/v1alpha1/` | CRD 타입. 기본값과 검증은 kubebuilder 마커로 정의한다. |
| `charts/config-reload-operator/` | Helm 차트. 공식 설치 경로다. |
| `config/` | controller-gen이 만든 CRD와 RBAC, 샘플 정책. |
| `demo/nginx/` | 데모 이미지(NGINX + watcher)와 매니페스트. |
| `docs/` | 추가 문서. 설계 이유는 [docs/design.md](docs/design.md), 이미지와 릴리스 방법은 [docs/images.md](docs/images.md). |
| `notes/`, `reference/` | 개인 학습 노트와 작업용 참고 파일. git에 들어가지 않는다. |

### 꼭 알아야 할 동작

**watcher**

- **앱이 PID 1이다.** 이미지의 시작 명령은 `watcher-entrypoint.sh`다. `CONFIG_RELOAD_DIR`가 있으면 같은 폴더의 `watcher`를 백그라운드로 띄우고, `exec "$@"`로 앱이 된다. watcher는 앱의 자식이고, 시그널을 부모에게 보낸다.
- **watcher의 어떤 실패도 앱을 내리면 안 된다.** watcher 실행 파일이 없어도 스크립트는 앱을 띄운다. 감시가 멈추면 watcher가 스스로 다시 시도하고, panic도 잡는다. 스크립트에 앱 시작을 막을 수 있는 것을 넣지 않는다.
- **`CONFIG_RELOAD_DIR`가 없으면** 스크립트는 watcher를 띄우지 않는다. 정책이 없어도 이미지가 그대로 동작해야 한다.
- **watcher는 앱을 띄우지 않는다.** 인자 없이 실행하면 감시하고, `--healthcheck`는 watch 프로세스가 살아 있는지 알려 준다. 그 밖의 인자(0.1 방식의 `watcher -- 명령`)나 PID 1로 실행되면 시작 스크립트를 쓰라는 메시지를 내고 끝난다.
- **환경 변수 계약**: `CONFIG_RELOAD_DIR`, `CONFIG_RELOAD_SIGNAL`, `CONFIG_RELOAD_DEBOUNCE_SECONDS`, `CONFIG_RELOAD_VALIDATE_COMMAND`(JSON 문자열 배열). 이름이 `cmd/watcher/main.go`와 `internal/controller/pod_webhook.go` 양쪽에 상수로 따로 정의되어 있고, 스크립트에도 `CONFIG_RELOAD_DIR`가 있다. 하나를 바꾸면 다른 쪽도 같이 바꾼다.

**웹훅**

- **주입은 Pod에 한다.** `PodInjector`는 애노테이션 `reload.nagisatosanae.github.io/inject: "true"`가 있는 Pod가 만들어질 때, 그 Deployment를 가리키는 정책을 읽어 ConfigMap volume, mount, env 네 개를 넣는다. Deployment는 고치지 않는다. 애노테이션은 Deployment의 `metadata`가 아니라 Pod 템플릿에 붙인다.
- **웹훅은 정책만 읽는다.** Deployment 이름은 Pod 자신에게서 알아낸다(소유자 ReplicaSet 이름에서 `pod-template-hash`를 뗀다).
- **전부 아니면 전무다.** 정책 하나라도 적용할 수 없으면 아무것도 넣지 않는다. 문제의 종류: 정책 없음, container 없음, `CONFIG_RELOAD_` env를 사용자가 이미 씀, mount 경로에 다른 것이 있음(같은 ConfigMap이면 그대로 쓴다), 한 container를 두 정책이 가리킴, Deployment의 Pod가 아님, 정책을 읽지 못함.
- **`failurePolicy`가 문제를 어떻게 다룰지 정한다.** `Fail`(기본)이면 Pod를 거부하고 이유를 메시지에 적는다. `Ignore`면 사용자가 쓴 그대로 통과시킨다. 차트 값 `webhook.failurePolicy` 하나가 웹훅 등록(API server가 웹훅에 닿지 못했을 때)과 manager 인자 `--webhook-failure-policy`(웹훅이 문제를 발견했을 때) 양쪽에 들어간다. 둘이 어긋나면 안 된다.
- **주입한 Pod에는 표시를 남긴다.** 애노테이션 `reload.nagisatosanae.github.io/injected`에 `정책이름@세대@UID`를 적는다. controller가 이것으로 센다. UID는 지웠다가 같은 이름으로 다시 만든 정책을 구별한다. 0.2.0이 남긴 `정책이름@세대`도 읽고, 그때는 세대만 비교한다. 이 표시가 이미 있는 Pod는 다시 주입하지 않는다.
- **Deployment는 apply할 때 검사한다.** `DeploymentChecker`는 Pod 템플릿으로 Pod를 만들어 `PodInjector.injectFor`를 돌려 보고 결과를 버린다. Pod 웹훅과 같은 코드이므로 두 판단이 어긋나지 않는다. `Fail`이면 거부하고 `Ignore`면 경고를 낸다. 삭제 중인 Deployment는 항상 통과시킨다.
- **웹훅의 이름들이 두 곳에 있다**: 애노테이션, 경로 두 개(`/mutate--v1-pod`, `/validate-apps-v1-deployment`), 필터 식이 `internal/controller/`의 Go 파일과 `charts/config-reload-operator/templates/webhook.yaml`에 따로 적혀 있다. 하나를 바꾸면 다른 쪽도 같이 바꾼다. `TestChartMatchesTheWebhook`이 어긋남을 잡는다.
- **웹훅은 인증서가 있을 때만 켜진다.** manager는 `--webhook-cert-path`가 있을 때만 웹훅을 등록한다. 인증서는 Helm이 만든다. cert-manager는 쓰지 않는다.

**controller**

- **controller는 정책의 status만 쓴다.** Deployment와 Pod는 읽기만 하고, 권한도 읽기뿐이다. 반복 reconcile이 무언가를 쓰면 버그다.
- **status는 Pod를 세어 적는다.** `status.pods`는 그 Deployment의 Pod 중 애노테이션이 있는 것, `status.injectedPods`는 그중 정책의 현재 세대와 UID 표시를 가진 것이다. Ready의 reason은 `Injected`, `Accepted`, `NotInjected`, `Outdated`, `WorkloadNotFound`, `ContainerNotFound`, `NotOptedIn`이다.
- **Pod가 생기거나 없어지면 다시 본다.** Pod는 metadata만 캐시에 둔다. Ready가 아니면 30초, Ready면 5분 뒤에도 다시 본다.
- **돌고 있는 것은 건드리지 않는다.** 정책을 지워도, 정책을 고쳐도 돌고 있는 Pod는 그대로다. 걷어내기도 다시 띄우기도 하지 않는다. finalizer도 없다. 0.1이 붙인 finalizer `reload.nagisatosanae.github.io/revert`는 떼어 주기만 한다.

**배포**

- **웹훅과 controller는 Deployment가 따로다.** 이미지는 같고, 웹훅 쪽은 `--controller=false`로 띄운다. 서로 없어도 동작해야 한다. 서비스 계정과 권한도 따로다.
- **웹훅도 controller도 reload 경로에 없다.** 둘 다 내려가 있어도 이미 뜬 Pod의 reload는 계속 동작해야 한다.
- **controller Deployment의 이름과 selector는 바꾸지 않는다.** selector는 나중에 바꿀 수 없어서, 바꾸면 upgrade가 깨진다.

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
- controller의 RBAC 마커를 고치면 차트의 `templates/rbac.yaml`도 같은 규칙으로 맞춘다. 웹훅용 권한은 차트에만 있다.
- watcher와 demo 이미지는 Docker 밖에서 빌드한 `bin/watcher`와 `cmd/watcher/watcher-entrypoint.sh`를 복사한다. 이미지를 만들기 전에 `./build.sh`를 먼저 실행한다. `.dockerignore`가 기본으로 모든 파일을 빼므로, 이미지에 넣을 파일은 거기에 다시 넣어 줘야 한다.
- 텍스트 파일은 LF로 저장한다(`.gitattributes`). 시작 스크립트가 CRLF면 container가 뜨지 않는다.
- e2e 테스트는 없다. 전체 흐름은 README의 quick start로 직접 확인한다.
- `kubebuilder create ...`는 이 저장소에서 끝까지 돌지 않는다. `cmd/main.go`와 `test/e2e/`를 찾는데 둘 다 없다. 새 조각은 저장소의 구조에 맞춰 손으로 쓴다. 웹훅 설정도 `+kubebuilder:webhook` 마커가 아니라 차트 템플릿에 직접 쓴다.

## 테스트

- watcher: 표준 `testing` 패키지. kubelet의 `..data` 심볼릭 링크 교체를 임시 디렉터리에서 흉내 낸다. 시작 스크립트 테스트는 실제 스크립트를 돌리고, 테스트 바이너리가 watcher 역할을 한다.
- controller와 웹훅: Ginkgo + Gomega + envtest(실제 API 서버와 etcd). `-short`를 주면 envtest 스위트는 건너뛴다. 웹훅과 Deployment 검사는 envtest API 서버가 실제로 부른다. envtest에는 ReplicaSet controller가 없으므로, 테스트가 ReplicaSet이 만드는 모양의 Pod를 직접 만든다.
- 동작을 바꾸면 테스트도 같이 바꾼다. README의 테스트 개수 표와 어긋나면 표도 맞춘다.

## 브랜치와 릴리스

- 기본 브랜치는 `main`, 평소 작업은 `develop`에서 한다.
- GitHub Actions: push와 PR마다 `test.yml`, `lint.yml`이 돈다. `v*` 태그를 push하면 `images.yml`이 `watcher`, `demo-nginx`, `manager` 이미지를 ghcr.io에 올린다.
- 릴리스할 때 버전을 맞춰야 하는 곳: `charts/config-reload-operator/Chart.yaml`(`version`, `appVersion`), `demo/nginx/k8s.yaml`의 이미지 태그, README와 `docs/images.md`와 `cmd/watcher/Dockerfile` 주석의 watcher 이미지 태그.
- operator와 watcher는 같은 버전끼리 쓴다. env 이름이 둘 사이의 약속이기 때문이다.

## 작업 규칙

- **커밋과 push는 사용자가 직접 한다.** `git commit`, `git push`, `git tag`를 실행하지 않는다.
- **저장소에 들어가는 글은 영어로 쓴다.** 코드 주석, 커밋 메시지, README, docs 모두 짧고 쉬운 문장의 영어다. 이 파일만 예외로 한글이다.
- **`notes/`의 내용은 커밋되는 파일에 옮기지 않는다.**
- **README는 요청받은 부분만 고친다.** 섹션이나 목록을 임의로 늘리지 않는다.
- 고정된 이름을 바꾸지 않는다: 저장소와 차트 `config-reload-operator`, kind `ConfigReloadPolicy`, 바이너리 `manager`와 `watcher`.
- v1 범위 밖의 것(StatefulSet, DaemonSet, Secret, HTTP reload, 다른 네임스페이스 참조, label selector, sidecar, operator가 Pod를 다시 띄우는 일)은 요청이 없으면 만들지 않는다.
