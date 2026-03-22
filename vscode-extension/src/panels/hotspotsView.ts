import * as vscode from 'vscode';
import * as path from 'path';

interface HotspotItem {
  path: string;
  commits: number;
  authors: number;
  churnScore: number;
  lastAuthor: string;
}

export class HotspotsProvider implements vscode.TreeDataProvider<HotspotNode> {
  private _onDidChangeTreeData = new vscode.EventEmitter<HotspotNode | undefined>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;

  private items: HotspotItem[] = [];

  refresh(items: HotspotItem[]): void {
    this.items = items;
    this._onDidChangeTreeData.fire(undefined);
  }

  getTreeItem(element: HotspotNode): vscode.TreeItem {
    return element;
  }

  getChildren(): HotspotNode[] {
    return this.items.map((item) => {
      const label = path.basename(item.path);
      const node = new HotspotNode(
        label,
        `${item.commits} commits, churn=${item.churnScore.toFixed(1)}`,
        item.path,
        vscode.TreeItemCollapsibleState.None
      );

      node.iconPath = new vscode.ThemeIcon(
        'flame',
        item.churnScore > 50
          ? new vscode.ThemeColor('charts.red')
          : item.churnScore > 20
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
              item.path
            )
          ),
        ],
      };

      return node;
    });
  }
}

class HotspotNode extends vscode.TreeItem {
  constructor(
    public readonly label: string,
    public readonly description: string,
    public readonly filePath: string,
    public readonly collapsibleState: vscode.TreeItemCollapsibleState
  ) {
    super(label, collapsibleState);
    this.tooltip = `${filePath}\n${description}`;
  }
}
