import * as vscode from 'vscode';

export function createStatusBar(): vscode.StatusBarItem {
  const item = vscode.window.createStatusBarItem(
    vscode.StatusBarAlignment.Left,
    100
  );
  item.command = 'mantis.refreshDiagnostics';
  updateStatusBar(item, false);
  item.show();
  return item;
}

export function updateStatusBar(
  item: vscode.StatusBarItem,
  connected: boolean
): void {
  if (connected) {
    item.text = '$(check) Mantis: Connected';
    item.backgroundColor = undefined;
    item.tooltip = 'Mantis LSP is running. Click to refresh diagnostics.';
  } else {
    item.text = '$(error) Mantis: Disconnected';
    item.backgroundColor = new vscode.ThemeColor(
      'statusBarItem.errorBackground'
    );
    item.tooltip = 'Mantis LSP is not running. Click to retry.';
  }
}
