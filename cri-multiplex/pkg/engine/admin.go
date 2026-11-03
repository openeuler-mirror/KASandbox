package engine

import (
	"context"
	"time"
)

// ---- 中性领域类型 ----

// VolumeMount 描述一个卷挂载。
type VolumeMount struct {
	ID   string // 卷 ID
	Name string // 卷名
	Path string // 挂载路径
	Type string // 卷类型
}

// NetworkPolicy 是跨后端的网络策略子集。
type NetworkPolicy struct {
	AllowedCIDRs       []string // 允许出网的 CIDR
	DeniedCIDRs        []string // 禁止出网的 CIDR
	AllowedDomains     []string // 允许出网的域名
	TrafficAccessToken string   // 入网鉴权 token
	MaskRequestHost    *string  // 是否改写 Host 头；nil 表示未指定
}

// SandboxSpec 是创建沙箱的通用描述；后端专有字段放入 Extra。
type SandboxSpec struct {
	SandboxID             string            // 必填；同时作为 CRI id 与产品 id，幂等键
	TemplateID            string            // 模板 ID
	BuildID               string            // 构建 ID
	Image                 string            // 镜像引用；非模板型后端使用
	Alias                 string            // 沙箱别名
	VCPU                  int64             // vCPU 数量
	RamMB                 int64             // 内存 MiB
	DiskMB                int64             // 磁盘 MiB
	MaxSandboxLengthHours int64             // 最长存活小时数；0 表示不限制
	AllowInternet         *bool             // 是否允许出网；nil 表示由后端默认
	EnvVars               map[string]string // 环境变量
	Metadata              map[string]string // 业务元数据
	VolumeMounts          []VolumeMount     // 卷挂载
	Network               *NetworkPolicy    // 网络策略；nil 表示不设置
	Extra                 map[string]string // 后端专有字段，值为 JSON 字符串
	StartTime             *time.Time        // 期望开始时间；nil 表示不指定
	EndTime               *time.Time        // 期望结束时间；nil 表示不指定
}

// SandboxCreateResult 是创建沙箱的结果。
type SandboxCreateResult struct {
	SandboxID string // 产品侧沙箱 ID
	ClientID  string // 后端客户端 ID
	HostIP    string // 沙箱宿主 IP
}

// SandboxUpdate 是要更新的字段；nil 表示不修改。
type SandboxUpdate struct {
	EndTime *time.Time     // 新的结束时间
	Egress  *NetworkPolicy // 新的出网策略
}

// CachedBuild 是一条构建缓存记录。
type CachedBuild struct {
	BuildID        string    // 构建 ID
	ExpirationTime time.Time // 缓存过期时间
}

// SandboxRef 定位一个沙箱；两个字段至少填一个。
type SandboxRef struct {
	CRISandboxID string // CRI PodSandbox ID
	SandboxID    string // 产品侧稳定 ID
}

// AdminOpOptions 是管理操作的公共参数。
type AdminOpOptions struct {
	OperationID string            // 幂等键
	Timeout     time.Duration     // 操作超时
	Metadata    map[string]string // 后端专有参数
}

// AdminOpResult 是管理操作的执行结果。
type AdminOpResult struct {
	SnapshotPersisted bool      // 快照是否已持久化
	VMResumed         bool      // VM 是否已恢复运行
	StartedAt         time.Time // 开始时间
	FinishedAt        time.Time // 结束时间
}

// SandboxHostPort 是一条端口映射事实；框架层不依赖 util.PortMapping。
type SandboxHostPort struct {
	HostPort    int // 宿主端口
	SandboxPort int // 沙箱内端口
}

// SandboxRuntime 是节点运行事实的中性视图。
type SandboxRuntime struct {
	Backend      string             // 所属后端
	CRISandboxID string             // CRI 沙箱 ID
	SandboxID    string             // 产品侧沙箱 ID
	State        string             // 运行状态
	TemplateID   string             // 模板 ID
	BuildID      string             // 构建 ID
	PodName      string             // Pod 名
	PodNamespace string             // Pod 命名空间
	PodUID       string             // Pod UID
	NodeName     string             // 节点名
	PodIP        string             // Pod IP
	HostIP       string             // 宿主 IP
	NetNSPath    string             // netns 路径
	HostPorts    []SandboxHostPort  // 端口映射
	ActiveOps    []SandboxOperation // 进行中的操作
	Extra        map[string]string  // 后端专有事实
	StartTime    *time.Time         // 启动时间；nil 表示未知
	EndTime      *time.Time         // 结束时间；nil 表示未知
}

// SandboxOperation 是一条进行中的管理操作。
type SandboxOperation struct {
	OperationID string    // 幂等键
	Action      string    // 操作类型：pause/checkpoint...
	State       string    // 操作状态
	Error       string    // 错误信息；成功时为空
	StartedAt   time.Time // 开始时间
	FinishedAt  time.Time // 结束时间
}

// ---- Admin 能力接口：方法名统一 Admin 前缀 ----

// AdminLifecycle 是沙箱生命周期管理能力（5 个方法）。
type AdminLifecycle interface {
	AdminCreateSandbox(ctx context.Context, spec SandboxSpec) (*SandboxCreateResult, error)
	AdminUpdateSandbox(ctx context.Context, sandboxID string, upd SandboxUpdate) error
	AdminListSandboxes(ctx context.Context) ([]*SandboxRuntime, error)
	AdminDeleteSandbox(ctx context.Context, sandboxID string) error
	AdminListCachedBuilds(ctx context.Context) ([]CachedBuild, error)
}

// AdminPauser 是暂停沙箱的能力。
type AdminPauser interface {
	AdminPauseSandbox(ctx context.Context, ref SandboxRef, opts AdminOpOptions) (*AdminOpResult, error)
}

// AdminCheckpointer 是创建检查点的能力。
type AdminCheckpointer interface {
	AdminCheckpointSandbox(ctx context.Context, ref SandboxRef, opts AdminOpOptions) (*AdminOpResult, error)
}

// AdminRuntimeDescriber 是查询节点运行事实的能力。
type AdminRuntimeDescriber interface {
	AdminGetSandboxRuntime(ctx context.Context, ref SandboxRef) (*SandboxRuntime, error)
}

// AdminEngine 是 8 个 Admin 方法全部实现的组合接口，便于编译期断言。
type AdminEngine interface {
	AdminLifecycle
	AdminPauser
	AdminCheckpointer
	AdminRuntimeDescriber
}
