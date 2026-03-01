import * as vscode from 'vscode';
import { runMantis, getWorkspaceRoot } from '../mantisRunner';

export class DependencyTreeProvider
  implements vscode.TreeDataProvider<DependencyItem>
{
  private _onDidChangeTreeData = new vscode.EventEmitter<
    DependencyItem | undefined | void
  >();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;

  private _currentFile: string | undefined;

  refresh(file?: string): void {
    this._currentFile = file;
    this._onDidChangeTreeData.fire();
  }

  getTreeItem(element: DependencyItem): vscode.TreeItem {
    return element;
  }

  async getChildren(element?: DependencyItem): Promise<DependencyItem[]> {
    if (element) {
      return [];
    }

    const file = this._currentFile;
    if (!file) {
      return [
        new DependencyItem(
          'Open a file to see dependencies',
          '',
          vscode.TreeItemCollapsibleState.None,
          'info'
        ),
      ];
    }

    const root = getWorkspaceRoot();
    if (!root) {
      return [];
    }

    // Use relative path for display
    const relativePath = file.startsWith(root)
      ? file.slice(root.length + 1)
      : file;

    const result = await runMantis(
      ['find', relativePath, '--format', 'json'],
      root
    );

    if (result.exitCode !== 0 || !result.stdout.trim()) {
      return [
        new DependencyItem(
          'No importers found (or run mantis init)',
          '',
          vscode.TreeItemCollapsibleState.None,
          'info'
        ),
      ];
    }

    try {
      const paths: string[] = JSON.parse(result.stdout);
      if (!paths.length) {
        return [
          new DependencyItem(
            'No importers found',
            '',
            vscode.TreeItemCollapsibleState.None,
            'info'
          ),
        ];
      }
      return paths.map(
        (p) =>
          new DependencyItem(
            p.startsWith(root) ? p.slice(root.length + 1) : p,
            p,
            vscode.TreeItemCollapsibleState.None,
            'file'
          )
      );
    } catch {
      return [
        new DependencyItem(
          result.stdout.trim().split('\n')[0] ?? 'Error parsing output',
          '',
          vscode.TreeItemCollapsibleState.None,
          'info'
        ),
      ];
    }
  }
}

export class DependencyItem extends vscode.TreeItem {
  constructor(
    public readonly label: string,
    public readonly filePath: string,
    public readonly collapsibleState: vscode.TreeItemCollapsibleState,
    public readonly kind: 'file' | 'info'
  ) {
    super(label, collapsibleState);
    if (kind === 'file' && filePath) {
      this.tooltip = filePath;
      this.command = {
        command: 'vscode.open',
        title: 'Open File',
        arguments: [vscode.Uri.file(filePath)],
      };
      this.iconPath = new vscode.ThemeIcon('file');
    } else {
      this.iconPath = new vscode.ThemeIcon('info');
    }
  }
}
