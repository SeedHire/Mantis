import * as vscode from 'vscode';
import { LanguageClient } from 'vscode-languageclient/node';
import { ImpactProvider } from './panels/impactView';

export function registerCommands(
  context: vscode.ExtensionContext,
  getClient: () => LanguageClient | undefined,
  impactProvider: ImpactProvider
): void {
  // mantis.impact — pick a symbol, run impact analysis, show in tree view.
  context.subscriptions.push(
    vscode.commands.registerCommand('mantis.impact', async () => {
      const client = getClient();
      if (!client) {
        vscode.window.showWarningMessage('Mantis LSP is not connected.');
        return;
      }

      const target = await vscode.window.showInputBox({
        prompt: 'Enter symbol or file path to analyze impact',
        placeHolder: 'e.g. HandleRequest or internal/api/handler.go',
      });
      if (!target) {
        return;
      }

      try {
        const result = await client.sendRequest('mantis/impact', {
          target,
          depth: 5,
        });
        impactProvider.refresh(result as any);
        vscode.commands.executeCommand('mantis-impact.focus');
      } catch (err: any) {
        vscode.window.showErrorMessage(`Impact analysis failed: ${err.message}`);
      }
    })
  );

  // mantis.findSymbol — input a name, jump to definition.
  context.subscriptions.push(
    vscode.commands.registerCommand('mantis.findSymbol', async () => {
      const client = getClient();
      if (!client) {
        vscode.window.showWarningMessage('Mantis LSP is not connected.');
        return;
      }

      const name = await vscode.window.showInputBox({
        prompt: 'Enter symbol name to find',
        placeHolder: 'e.g. NewServer',
      });
      if (!name) {
        return;
      }

      const editor = vscode.window.activeTextEditor;
      if (!editor) {
        return;
      }

      try {
        const locations = await client.sendRequest('textDocument/definition', {
          textDocument: { uri: editor.document.uri.toString() },
          position: { line: 0, character: 0 },
        });

        if (Array.isArray(locations) && locations.length > 0) {
          const loc = locations[0];
          const uri = vscode.Uri.parse(loc.uri);
          const pos = new vscode.Position(loc.range.start.line, loc.range.start.character);
          const doc = await vscode.workspace.openTextDocument(uri);
          await vscode.window.showTextDocument(doc, {
            selection: new vscode.Range(pos, pos),
          });
        }
      } catch (err: any) {
        vscode.window.showErrorMessage(`Find symbol failed: ${err.message}`);
      }
    })
  );

  // mantis.coupling — pick a file, show coupling in output channel.
  context.subscriptions.push(
    vscode.commands.registerCommand('mantis.coupling', async () => {
      const client = getClient();
      if (!client) {
        vscode.window.showWarningMessage('Mantis LSP is not connected.');
        return;
      }

      const editor = vscode.window.activeTextEditor;
      const defaultFile = editor
        ? vscode.workspace.asRelativePath(editor.document.uri)
        : '';

      const file = await vscode.window.showInputBox({
        prompt: 'Enter file path to analyze coupling',
        value: defaultFile,
      });
      if (!file) {
        return;
      }

      try {
        const result: any[] = await client.sendRequest('mantis/coupling', {
          file,
          limit: 10,
        });

        const channel = vscode.window.createOutputChannel('Mantis Coupling');
        channel.clear();
        channel.appendLine(`Temporal coupling for: ${file}\n`);

        for (const item of result) {
          const pct = (item.coupling * 100).toFixed(0);
          channel.appendLine(
            `  ${item.file.padEnd(50)}  ${item.coChanges} co-changes  ${pct}%`
          );
        }
        channel.show();
      } catch (err: any) {
        vscode.window.showErrorMessage(`Coupling analysis failed: ${err.message}`);
      }
    })
  );

  // mantis.refreshDiagnostics — restart client to re-trigger diagnostics.
  context.subscriptions.push(
    vscode.commands.registerCommand('mantis.refreshDiagnostics', async () => {
      const client = getClient();
      if (!client) {
        vscode.window.showWarningMessage('Mantis LSP is not connected.');
        return;
      }
      // Re-save active editor to trigger didSave → diagnostics.
      const editor = vscode.window.activeTextEditor;
      if (editor) {
        await editor.document.save();
      }
    })
  );

  // mantis.init — run mantis init in integrated terminal.
  context.subscriptions.push(
    vscode.commands.registerCommand('mantis.init', () => {
      const terminal = vscode.window.createTerminal('Mantis');
      terminal.sendText('mantis init');
      terminal.show();
    })
  );
}
