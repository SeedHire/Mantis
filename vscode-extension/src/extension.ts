import * as vscode from 'vscode';
import * as path from 'path';
import {
  LanguageClient,
  LanguageClientOptions,
  ServerOptions,
  TransportKind,
} from 'vscode-languageclient/node';
import { HotspotsProvider } from './panels/hotspotsView';
import { DeadCodeProvider } from './panels/deadCodeView';
import { ImpactProvider } from './panels/impactView';
import { createStatusBar, updateStatusBar } from './statusBar';
import { registerCommands } from './commands';

let client: LanguageClient | undefined;
let statusBarItem: vscode.StatusBarItem;

export function activate(context: vscode.ExtensionContext) {
  statusBarItem = createStatusBar();
  context.subscriptions.push(statusBarItem);

  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (!workspaceRoot) {
    updateStatusBar(statusBarItem, false);
    return;
  }

  const graphDb = path.join(workspaceRoot, '.mantis', 'graph.db');

  // Check if graph.db exists and start LSP.
  vscode.workspace.fs.stat(vscode.Uri.file(graphDb)).then(
    () => startClient(context, workspaceRoot),
    () => {
      updateStatusBar(statusBarItem, false);
      vscode.window
        .showInformationMessage(
          'Mantis graph not found. Run "mantis init" to index this project.',
          'Run mantis init'
        )
        .then((choice) => {
          if (choice === 'Run mantis init') {
            const terminal = vscode.window.createTerminal('Mantis');
            terminal.sendText('mantis init');
            terminal.show();
          }
        });
    }
  );

  // Watch for graph.db creation.
  const watcher = vscode.workspace.createFileSystemWatcher(
    new vscode.RelativePattern(workspaceRoot, '.mantis/graph.db')
  );
  watcher.onDidCreate(() => {
    if (!client) {
      startClient(context, workspaceRoot);
    }
  });
  context.subscriptions.push(watcher);

  // Register tree views.
  const hotspotsProvider = new HotspotsProvider();
  const deadCodeProvider = new DeadCodeProvider();
  const impactProvider = new ImpactProvider();

  context.subscriptions.push(
    vscode.window.registerTreeDataProvider('mantis-hotspots', hotspotsProvider),
    vscode.window.registerTreeDataProvider('mantis-deadcode', deadCodeProvider),
    vscode.window.registerTreeDataProvider('mantis-impact', impactProvider)
  );

  // Register commands.
  registerCommands(context, () => client, impactProvider);
}

function startClient(
  context: vscode.ExtensionContext,
  workspaceRoot: string
) {
  const config = vscode.workspace.getConfiguration('mantis');
  const binaryPath = config.get<string>('binaryPath', 'mantis');

  const serverOptions: ServerOptions = {
    command: binaryPath,
    args: ['lsp'],
    options: { cwd: workspaceRoot },
    transport: TransportKind.stdio,
  };

  const clientOptions: LanguageClientOptions = {
    documentSelector: [
      { scheme: 'file', language: 'go' },
      { scheme: 'file', language: 'typescript' },
      { scheme: 'file', language: 'typescriptreact' },
      { scheme: 'file', language: 'python' },
    ],
    synchronize: {
      fileEvents: vscode.workspace.createFileSystemWatcher('**/*.{go,ts,tsx,py}'),
    },
  };

  client = new LanguageClient(
    'mantis',
    'Mantis LSP',
    serverOptions,
    clientOptions
  );

  client.start().then(
    () => updateStatusBar(statusBarItem, true),
    (err) => {
      updateStatusBar(statusBarItem, false);
      vscode.window.showErrorMessage(`Mantis LSP failed to start: ${err.message}`);
    }
  );

  context.subscriptions.push({ dispose: () => client?.stop() });
}

export function deactivate(): Thenable<void> | undefined {
  if (client) {
    return client.stop();
  }
  return undefined;
}
