import * as vscode from 'vscode';
import * as path from 'path';

interface DeadCodeItem {
  name: string;
  type: string;
  filePath: string;
  line: number;
}

export class DeadCodeProvider implements vscode.TreeDataProvider<DeadCodeNode> {
  private _onDidChangeTreeData = new vscode.EventEmitter<DeadCodeNode | undefined>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;

  private items: DeadCodeItem[] = [];

  refresh(items: DeadCodeItem[]): void {
    this.items = items;
    this._onDidChangeTreeData.fire(undefined);
  }

  getTreeItem(element: DeadCodeNode): vscode.TreeItem {
    return element;
  }

  getChildren(element?: DeadCodeNode): DeadCodeNode[] {
    if (element) {
      // Children of a file group.
      return this.items
        .filter((item) => item.filePath === element.filePath)
        .map((item) => {
          const node = new DeadCodeNode(
            `${item.name} (${item.type})`,
            `line ${item.line}`,
            item.filePath,
            item.line,
            vscode.TreeItemCollapsibleState.None
          );
          node.iconPath = new vscode.ThemeIcon('warning');
          node.command = {
            command: 'vscode.open',
            title: 'Go to Symbol',
            arguments: [
              vscode.Uri.file(
                path.join(
                  vscode.workspace.workspaceFolders?.[0]?.uri.fsPath || '',
                  item.filePath
                )
              ),
              { selection: new vscode.Range(item.line - 1, 0, item.line - 1, 0) },
            ],
          };
          return node;
        });
    }

    // Top-level: group by file.
    const files = new Map<string, number>();
    for (const item of this.items) {
      files.set(item.filePath, (files.get(item.filePath) || 0) + 1);
    }

    return Array.from(files.entries()).map(([filePath, count]) => {
      const node = new DeadCodeNode(
        path.basename(filePath),
        `${count} unused symbol${count > 1 ? 's' : ''}`,
        filePath,
        0,
        vscode.TreeItemCollapsibleState.Collapsed
      );
      node.iconPath = new vscode.ThemeIcon('file');
      return node;
    });
  }
}

class DeadCodeNode extends vscode.TreeItem {
  constructor(
    public readonly label: string,
    public readonly description: string,
    public readonly filePath: string,
    public readonly line: number,
    public readonly collapsibleState: vscode.TreeItemCollapsibleState
  ) {
    super(label, collapsibleState);
    this.tooltip = `${filePath}:${line}`;
  }
}
