import * as path from 'path';
import * as vscode from 'vscode';
import { LintViolation } from '../mantisRunner';

export class LintResultsProvider
  implements vscode.TreeDataProvider<LintItem>
{
  private _onDidChangeTreeData = new vscode.EventEmitter<
    LintItem | undefined | void
  >();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;

  private _violations: LintViolation[] = [];

  refresh(violations: LintViolation[]): void {
    this._violations = violations;
    this._onDidChangeTreeData.fire();
  }

  getTreeItem(element: LintItem): vscode.TreeItem {
    return element;
  }

  getChildren(element?: LintItem): LintItem[] {
    if (element) {
      return [];
    }

    if (!this._violations.length) {
      return [
        new LintItem(
          '✓ No violations',
          '',
          0,
          'info',
          vscode.TreeItemCollapsibleState.None
        ),
      ];
    }

    return this._violations.map((v) => {
      const [filePath, lineStr] = v.location.split(':');
      const line = parseInt(lineStr ?? '1', 10) || 1;
      const label = `${v.severity.toUpperCase()}: ${path.basename(filePath ?? v.location)}:${line}`;
      return new LintItem(
        label,
        filePath ?? v.location,
        line,
        v.severity,
        vscode.TreeItemCollapsibleState.None,
        v.message
      );
    });
  }
}

export class LintItem extends vscode.TreeItem {
  constructor(
    public readonly label: string,
    public readonly filePath: string,
    public readonly line: number,
    public readonly severity: string,
    public readonly collapsibleState: vscode.TreeItemCollapsibleState,
    public readonly detail?: string
  ) {
    super(label, collapsibleState);
    if (filePath && severity !== 'info') {
      this.tooltip = detail ? `${filePath}:${line}\n${detail}` : `${filePath}:${line}`;
      this.description = detail;
      this.command = {
        command: 'vscode.open',
        title: 'Open File',
        arguments: [
          vscode.Uri.file(filePath),
          { selection: new vscode.Range(line - 1, 0, line - 1, 0) },
        ],
      };
      this.iconPath = new vscode.ThemeIcon(
        severity === 'error' ? 'error' : 'warning'
      );
    } else {
      this.iconPath = new vscode.ThemeIcon('pass');
    }
  }
}
