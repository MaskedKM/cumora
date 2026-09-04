// workspacescontract —— 别名 glue:server 生成物裸引用的根包 types 符号
// (SessionBearerScopes 常量与 *Params 查询参数结构)。由
// contract-gen-go.sh 生成,勿手改。
package workspacescontract

import "github.com/MaskedKM/cumora/apps/server-go/internal/contract"

type ListWorkspaceFilesParams = contract.ListWorkspaceFilesParams
type ReadWorkspaceFileParams = contract.ReadWorkspaceFileParams
type ReadWorkspaceFileRawParams = contract.ReadWorkspaceFileRawParams
type WriteWorkspaceFileParams = contract.WriteWorkspaceFileParams

const SessionBearerScopes = contract.SessionBearerScopes
