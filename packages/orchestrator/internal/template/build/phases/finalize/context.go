package finalize

import (
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
)

func normalizeTemplateContext(meta metadata.Template, osType vmm.OsType) metadata.Template {
	if osType != vmm.OsWindows {
		return meta
	}

	meta.Context = normalizeCommandContext(meta.Context)
	if meta.Start != nil {
		meta.Start.Context = normalizeCommandContext(meta.Start.Context)
	}

	return meta
}

func normalizeCommandContext(cmdCtx metadata.Context) metadata.Context {
	cmdCtx = cmdCtx.WithOsType(string(vmm.OsWindows))
	cmdCtx.User = ""
	cmdCtx.WorkDir = nil

	return cmdCtx
}
