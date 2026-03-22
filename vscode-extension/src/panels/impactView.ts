import * as vscode from 'vscode';
import * as path from 'path';

interface ImpactFile {
  filePath: string;
  depth: number;
  risk: number;
}

interface ImpactData {
  target: string;
  totalFiles: number;
  files: ImpactFile[];
}

export class ImpactProvider implements vscode.TreeDataProvider<ImpactNode> {
  private _onDidChangeTreeData = new vscode.EventEmitter<ImpactNode | undefined>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;

  private data: ImpactData | null = null;

  refresh(data: ImpactData): void {
    this.data = data;
    this._onDidChangeTreeData.fire(undefined);
  }

  getTreeItem(element: ImpactNode): vscode.TreeItem {
    return element;
  }

  getChildren(element?: ImpactNode): ImpactNode[] {
    if (!this.data) {
      return [];
    }

    if (element) {
      // Children of a depth group.
      return this.data.files
        .filter((f) => f.depth === element.depth)
        .map((f) => {
          const label = path.basename(f.filePath);
          const node = new ImpactNode(
            label,
            `risk: ${f.risk}/10`,
            f.depth,
            vscode.TreeItemCollapsibleState.None
          );

          node.iconPath = new vscode.ThemeIcon(
            'circle-filled',
            f.risk > 7
              ? new vscode.ThemeColor('charts.red')
              : f.risk > 4
              ? new vscode.ThemeColor('charts.yellow')
              : new vscode.ThemeColor('charts.green')
          );

          node.command = {
            command: 'vscode.open',
            title: 'Open File',
            arguments: [
              vscode.Uri.file(
                path.join(
                  vscode.workspace.workspaceFolders?.[0]?.uri.fsPath || '',
                  f.filePath
                )
              ),
            ],
          };

          return node;
        });
    }

    // Top-level: group by depth.
    const depths = new Set<number>();
    for (const f of this.data.files) {
      depths.add(f.depth);
    }

    return Array.from(depths)
      .sort((a, b) => a - b)
      .map((depth) => {
        const count = this.data!.files.filter((f) => f.depth === depth).length;
        return new ImpactNode(
          `Depth ${depth}`,
          `${count} file${count > 1 ? 's' : ''}`,
          depth,
          vscode.TreeItemCollapsibleState.Expanded
        );
      });
  }
}

class ImpactNode extends vscode.TreeItem {
  constructor(
    public readonly label: string,
    public readonly description: string,
    public readonly depth: number,
    public readonly collapsibleState: vscode.TreeItemCollapsibleState
  ) {
    super(label, collapsibleState);
  }
}
