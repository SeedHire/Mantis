import * as path from 'path';
import * as vscode from 'vscode';
import {
  runMantis,
  getWorkspaceRoot,
  parseLintOutput,
  parseFindOutput,
  LintViolation,
} from './mantisRunner';
import { DependencyTreeProvider } from './providers/dependencyTreeProvider';
import { LintResultsProvider } from './providers/lintResultsProvider';

let diagnosticCollection: vscode.DiagnosticCollection;
let statusBarItem: vscode.StatusBarItem;

export function activate(context: vscode.ExtensionContext): void {
  diagnosticCollection =
    vscode.languages.createDiagnosticCollection('mantis');
  context.subscriptions.push(diagnosticCollection);

  statusBarItem = vscode.window.createStatusBarItem(
    vscode.StatusBarAlignment.Left,
    100
  );
  statusBarItem.text = '$(symbol-structure) Mantis';
  statusBarItem.command = 'mantis.runLint';
  statusBarItem.tooltip = 'Run Mantis architecture lint';
  statusBarItem.show();
  context.subscriptions.push(statusBarItem);

  // Sidebar providers
  const depTreeProvider = new DependencyTreeProvider();
  const lintProvider = new LintResultsProvider();

  context.subscriptions.push(
    vscode.window.registerTreeDataProvider(
      'mantis.dependencyTree',
      depTreeProvider
    ),
    vscode.window.registerTreeDataProvider(
      'mantis.lintResults',
      lintProvider
    )
  );

  // Refresh dependency tree when the active editor changes
  context.subscriptions.push(
    vscode.window.onDidChangeActiveTextEditor((editor) => {
      if (editor?.document.uri.scheme === 'file') {
        depTreeProvider.refresh(editor.document.uri.fsPath);
      }
    })
  );

  // Auto-lint on save
  context.subscriptions.push(
    vscode.workspace.onDidSaveTextDocument(async (doc) => {
      const config = vscode.workspace.getConfiguration('mantis');
      if (config.get<boolean>('autoLint')) {
        await runLintAndUpdateUI(lintProvider);
      }
    })
  );

  // ── Commands ────────────────────────────────────────────────────────────────

  context.subscriptions.push(
    vscode.commands.registerCommand('mantis.init', async () => {
      const root = getWorkspaceRoot();
      if (!root) {
        vscode.window.showErrorMessage('Mantis: No workspace folder open.');
        return;
      }
      await vscode.window.withProgress(
        {
          location: vscode.ProgressLocation.Notification,
          title: 'Mantis: Indexing project…',
          cancellable: false,
        },
        async () => {
          const result = await runMantis(['init'], root);
          if (result.exitCode === 0) {
            vscode.window.showInformationMessage(
              `Mantis: ${result.stdout.trim().split('\n').pop() ?? 'Indexing complete.'}`
            );
          } else {
            vscode.window.showErrorMessage(
              `Mantis init failed: ${result.stderr || result.stdout}`
            );
          }
        }
      );
    })
  );

  context.subscriptions.push(
    vscode.commands.registerCommand('mantis.findSymbol', async () => {
      const editor = vscode.window.activeTextEditor;
      const symbol =
        editor?.document.getText(editor.selection) ||
        (await vscode.window.showInputBox({ prompt: 'Symbol name to find' }));
      if (!symbol) {
        return;
      }

      const root = getWorkspaceRoot();
      if (!root) {
        return;
      }

      await vscode.window.withProgress(
        {
          location: vscode.ProgressLocation.Notification,
          title: `Mantis: Finding usages of "${symbol}"…`,
          cancellable: false,
        },
        async () => {
          const result = await runMantis(
            ['find', symbol, '--format', 'json'],
            root
          );
          const files = parseFindOutput(result.stdout);
          if (!files.length) {
            vscode.window.showInformationMessage(
              `Mantis: No usages found for "${symbol}".`
            );
            return;
          }
          // Show a QuickPick to jump to any found file
          const items = files.map((f) => ({
            label: path.relative(root, f),
            description: f,
          }));
          const picked = await vscode.window.showQuickPick(items, {
            placeHolder: `${files.length} file(s) import "${symbol}" — pick one to open`,
          });
          if (picked?.description) {
            const doc = await vscode.workspace.openTextDocument(
              vscode.Uri.file(picked.description)
            );
            await vscode.window.showTextDocument(doc);
          }
        }
      );
    })
  );

  context.subscriptions.push(
    vscode.commands.registerCommand('mantis.bundleContext', async () => {
      const editor = vscode.window.activeTextEditor;
      const symbol =
        editor?.document.getText(editor.selection) ||
        (await vscode.window.showInputBox({
          prompt: 'Symbol name for context bundle',
        }));
      if (!symbol) {
        return;
      }

      const root = getWorkspaceRoot();
      if (!root) {
        return;
      }

      const config = vscode.workspace.getConfiguration('mantis');
      const tokens = config.get<number>('contextTokens') ?? 8000;
      const depth = config.get<number>('contextDepth') ?? 3;

      await vscode.window.withProgress(
        {
          location: vscode.ProgressLocation.Notification,
          title: `Mantis: Bundling context for "${symbol}"…`,
          cancellable: false,
        },
        async () => {
          const result = await runMantis(
            ['context', symbol, '--tokens', String(tokens), '--depth', String(depth)],
            root
          );
          if (result.exitCode !== 0) {
            vscode.window.showErrorMessage(
              `Mantis: ${result.stderr || result.stdout}`
            );
            return;
          }
          await vscode.env.clipboard.writeText(result.stdout);
          vscode.window.showInformationMessage(
            `Mantis: Context bundle for "${symbol}" copied to clipboard.`
          );
        }
      );
    })
  );

  context.subscriptions.push(
    vscode.commands.registerCommand('mantis.showImpact', async () => {
      const editor = vscode.window.activeTextEditor;
      const target =
        editor?.document.getText(editor.selection) ||
        (editor
          ? path.relative(getWorkspaceRoot() ?? '', editor.document.uri.fsPath)
          : undefined) ||
        (await vscode.window.showInputBox({ prompt: 'File or symbol to analyse' }));
      if (!target) {
        return;
      }

      const root = getWorkspaceRoot();
      if (!root) {
        return;
      }

      const result = await runMantis(['impact', target, '--risk'], root);
      const panel = vscode.window.createWebviewPanel(
        'mantisImpact',
        `Mantis Impact: ${path.basename(target)}`,
        vscode.ViewColumn.Beside,
        {}
      );
      panel.webview.html = wrapInHtml(
        `<pre>${escapeHtml(result.stdout || result.stderr)}</pre>`,
        `Impact: ${target}`
      );
    })
  );

  context.subscriptions.push(
    vscode.commands.registerCommand('mantis.openGraph', async () => {
      const root = getWorkspaceRoot();
      if (!root) {
        vscode.window.showErrorMessage('Mantis: No workspace folder open.');
        return;
      }

      await vscode.window.withProgress(
        {
          location: vscode.ProgressLocation.Notification,
          title: 'Mantis: Generating dependency graph…',
          cancellable: false,
        },
        async () => {
          const outPath = path.join(root, '.mantis', 'graph.html');
          const result = await runMantis(
            ['graph', '--out', outPath],
            root
          );
          if (result.exitCode !== 0) {
            vscode.window.showErrorMessage(
              `Mantis graph failed: ${result.stderr || result.stdout}`
            );
            return;
          }
          await vscode.env.openExternal(vscode.Uri.file(outPath));
        }
      );
    })
  );

  context.subscriptions.push(
    vscode.commands.registerCommand('mantis.runLint', async () => {
      await runLintAndUpdateUI(lintProvider);
    })
  );

  context.subscriptions.push(
    vscode.commands.registerCommand('mantis.findDead', async () => {
      const root = getWorkspaceRoot();
      if (!root) {
        return;
      }

      const result = await runMantis(['dead'], root);
      const panel = vscode.window.createWebviewPanel(
        'mantisDeadCode',
        'Mantis: Dead Code',
        vscode.ViewColumn.Beside,
        {}
      );
      panel.webview.html = wrapInHtml(
        `<pre>${escapeHtml(result.stdout || result.stderr)}</pre>`,
        'Dead Code'
      );
    })
  );

  // Refresh dependency tree for the currently open file on activation
  const activeEditor = vscode.window.activeTextEditor;
  if (activeEditor?.document.uri.scheme === 'file') {
    depTreeProvider.refresh(activeEditor.document.uri.fsPath);
  }
}

export function deactivate(): void {
  diagnosticCollection?.dispose();
  statusBarItem?.dispose();
}

// ── Helpers ──────────────────────────────────────────────────────────────────

async function runLintAndUpdateUI(
  lintProvider: LintResultsProvider
): Promise<void> {
  const root = getWorkspaceRoot();
  if (!root) {
    return;
  }

  statusBarItem.text = '$(sync~spin) Mantis: linting…';

  const result = await runMantis(['lint'], root);
  const violations = parseLintOutput(result.stdout);

  lintProvider.refresh(violations);
  updateDiagnostics(violations, root);

  if (!violations.length) {
    statusBarItem.text = '$(pass) Mantis';
    statusBarItem.backgroundColor = undefined;
  } else {
    const errorCount = violations.filter((v) => v.severity === 'error').length;
    statusBarItem.text = `$(error) Mantis: ${errorCount} violation${errorCount !== 1 ? 's' : ''}`;
    statusBarItem.backgroundColor = new vscode.ThemeColor(
      'statusBarItem.errorBackground'
    );
    if (errorCount > 0) {
      vscode.window.showWarningMessage(
        `Mantis lint: ${errorCount} architecture violation${errorCount !== 1 ? 's' : ''} found.`,
        'View'
      );
    }
  }
}

function updateDiagnostics(
  violations: LintViolation[],
  root: string
): void {
  diagnosticCollection.clear();

  const byFile = new Map<string, vscode.Diagnostic[]>();

  for (const v of violations) {
    const [filePart, lineStr] = v.location.split(':');
    if (!filePart) {
      continue;
    }
    const absPath = path.isAbsolute(filePart)
      ? filePart
      : path.join(root, filePart);
    const lineNum = Math.max(0, (parseInt(lineStr ?? '1', 10) || 1) - 1);
    const range = new vscode.Range(lineNum, 0, lineNum, 999);
    const severity =
      v.severity === 'error'
        ? vscode.DiagnosticSeverity.Error
        : vscode.DiagnosticSeverity.Warning;
    const diag = new vscode.Diagnostic(range, v.message, severity);
    diag.source = 'mantis';

    const existing = byFile.get(absPath) ?? [];
    existing.push(diag);
    byFile.set(absPath, existing);
  }

  for (const [file, diags] of byFile) {
    diagnosticCollection.set(vscode.Uri.file(file), diags);
  }
}

function wrapInHtml(content: string, title: string): string {
  return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>${escapeHtml(title)}</title>
  <style>
    body { font-family: var(--vscode-editor-font-family, monospace); padding: 16px; background: var(--vscode-editor-background); color: var(--vscode-editor-foreground); }
    pre  { white-space: pre-wrap; word-break: break-all; }
  </style>
</head>
<body>
  <h2>${escapeHtml(title)}</h2>
  ${content}
</body>
</html>`;
}

function escapeHtml(str: string): string {
  return str
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}
