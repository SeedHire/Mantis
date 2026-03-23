import * as vscode from 'vscode';
import { ChatProcess, ChatResponse } from '../chatProcess';

/**
 * Provides the Mantis AI Chat webview in the sidebar.
 */
export class ChatViewProvider implements vscode.WebviewViewProvider {
  public static readonly viewType = 'mantis-chat';

  private view?: vscode.WebviewView;
  private chatProcess: ChatProcess | null = null;
  private workspaceRoot: string;
  private currentRequestId = 0;

  constructor(private readonly extensionUri: vscode.Uri, workspaceRoot: string) {
    this.workspaceRoot = workspaceRoot;
  }

  resolveWebviewView(
    webviewView: vscode.WebviewView,
    _context: vscode.WebviewViewResolveContext,
    _token: vscode.CancellationToken
  ): void {
    this.view = webviewView;

    webviewView.webview.options = {
      enableScripts: true,
    };

    webviewView.webview.html = this.getHtmlContent();

    // Handle messages from the webview.
    webviewView.webview.onDidReceiveMessage((msg) => {
      switch (msg.type) {
        case 'send':
          this.handleUserMessage(msg.text);
          break;
        case 'cancel':
          this.chatProcess?.cancel();
          break;
        case 'command':
          this.handleSlashCommand(msg.name, msg.args);
          break;
      }
    });

    // Clean up on dispose.
    webviewView.onDidDispose(() => {
      this.chatProcess?.stop();
      this.chatProcess = null;
    });
  }

  private ensureProcess(): ChatProcess {
    if (this.chatProcess && this.chatProcess.isRunning) {
      return this.chatProcess;
    }

    const proc = new ChatProcess(this.workspaceRoot);

    proc.on('response', (resp: ChatResponse) => {
      // Forward directly — the webview JS handles resp.type (token/done/error/etc.)
      this.view?.webview.postMessage(resp);
    });

    proc.on('error', (err: Error) => {
      vscode.window.showErrorMessage(`Mantis chat error: ${err.message}`);
      this.view?.webview.postMessage({
        type: 'error',
        id: this.currentRequestId,
        error: err.message,
      });
    });

    proc.on('exit', (code: number | null) => {
      if (code !== 0 && code !== null) {
        this.view?.webview.postMessage({
          type: 'process-exit',
          code,
        });
      }
    });

    proc.start();
    this.chatProcess = proc;
    return proc;
  }

  private handleUserMessage(text: string): void {
    const trimmed = text.trim();
    if (!trimmed) {
      return;
    }

    // Detect slash commands.
    if (trimmed.startsWith('/')) {
      const parts = trimmed.substring(1).split(/\s+/);
      const name = parts[0];
      const args = parts.slice(1).join(' ');
      this.handleSlashCommand(name, args);
      return;
    }

    const proc = this.ensureProcess();
    this.currentRequestId = proc.sendChat(trimmed);
  }

  private handleSlashCommand(name: string, args?: string): void {
    const proc = this.ensureProcess();

    switch (name) {
      case 'reset':
      case 'brain':
      case 'conventions':
        this.currentRequestId = proc.sendCommand(name, args);
        break;
      default:
        // Unknown command — send as chat.
        this.currentRequestId = proc.sendChat(`/${name} ${args || ''}`.trim());
        break;
    }
  }

  private getHtmlContent(): string {
    return /*html*/ `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1.0">
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }

  body {
    font-family: var(--vscode-font-family, system-ui, sans-serif);
    font-size: var(--vscode-font-size, 13px);
    color: var(--vscode-foreground);
    background: var(--vscode-sideBar-background, var(--vscode-editor-background));
    display: flex;
    flex-direction: column;
    height: 100vh;
    overflow: hidden;
  }

  #chat-messages {
    flex: 1;
    overflow-y: auto;
    padding: 12px;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .message {
    max-width: 90%;
    padding: 8px 12px;
    border-radius: 8px;
    line-height: 1.45;
    word-wrap: break-word;
    white-space: pre-wrap;
  }

  .message.user {
    align-self: flex-end;
    background: var(--vscode-button-background, #0078d4);
    color: var(--vscode-button-foreground, #fff);
    border-bottom-right-radius: 2px;
  }

  .message.assistant {
    align-self: flex-start;
    background: var(--vscode-editorWidget-background, #252526);
    border: 1px solid var(--vscode-editorWidget-border, #454545);
    border-bottom-left-radius: 2px;
  }

  .message.status {
    align-self: center;
    font-size: 0.85em;
    opacity: 0.7;
    padding: 4px 8px;
  }

  .message.error {
    align-self: flex-start;
    background: var(--vscode-inputValidation-errorBackground, #5a1d1d);
    border: 1px solid var(--vscode-inputValidation-errorBorder, #be1100);
    border-radius: 4px;
  }

  .routing-badge {
    display: inline-block;
    font-size: 0.8em;
    padding: 2px 6px;
    border-radius: 3px;
    background: var(--vscode-badge-background, #4d4d4d);
    color: var(--vscode-badge-foreground, #fff);
    margin-bottom: 4px;
    opacity: 0.8;
  }

  .message pre {
    background: var(--vscode-textCodeBlock-background, #1e1e1e);
    padding: 8px;
    border-radius: 4px;
    overflow-x: auto;
    margin: 6px 0;
    font-family: var(--vscode-editor-font-family, 'Consolas', monospace);
    font-size: 0.92em;
  }

  .message code {
    font-family: var(--vscode-editor-font-family, 'Consolas', monospace);
    font-size: 0.92em;
    background: var(--vscode-textCodeBlock-background, #1e1e1e);
    padding: 1px 4px;
    border-radius: 3px;
  }

  .message pre code {
    background: none;
    padding: 0;
  }

  #input-area {
    display: flex;
    gap: 4px;
    padding: 8px 12px;
    border-top: 1px solid var(--vscode-panel-border, #454545);
    background: var(--vscode-sideBar-background, var(--vscode-editor-background));
  }

  #input-area textarea {
    flex: 1;
    resize: none;
    border: 1px solid var(--vscode-input-border, #3c3c3c);
    background: var(--vscode-input-background, #1e1e1e);
    color: var(--vscode-input-foreground, #ccc);
    padding: 6px 8px;
    border-radius: 4px;
    font-family: inherit;
    font-size: inherit;
    line-height: 1.4;
    min-height: 36px;
    max-height: 120px;
  }

  #input-area textarea:focus {
    outline: 1px solid var(--vscode-focusBorder, #007fd4);
    border-color: var(--vscode-focusBorder, #007fd4);
  }

  #input-area textarea::placeholder {
    color: var(--vscode-input-placeholderForeground, #666);
  }

  #send-btn {
    align-self: flex-end;
    padding: 6px 12px;
    background: var(--vscode-button-background, #0078d4);
    color: var(--vscode-button-foreground, #fff);
    border: none;
    border-radius: 4px;
    cursor: pointer;
    font-size: 13px;
    min-height: 36px;
  }

  #send-btn:hover {
    background: var(--vscode-button-hoverBackground, #1a8cff);
  }

  #send-btn:disabled {
    opacity: 0.5;
    cursor: default;
  }

  .typing-indicator {
    display: inline-block;
    opacity: 0.5;
  }

  .typing-indicator::after {
    content: '...';
    animation: dots 1.5s steps(4, end) infinite;
  }

  @keyframes dots {
    0%, 20% { content: ''; }
    40% { content: '.'; }
    60% { content: '..'; }
    80%, 100% { content: '...'; }
  }
</style>
</head>
<body>
  <div id="chat-messages">
    <div class="message status">Mantis AI Chat — type a message or use /commands</div>
  </div>
  <div id="input-area">
    <textarea id="input" rows="1" placeholder="Ask Mantis anything... (/ for commands)" autofocus></textarea>
    <button id="send-btn">Send</button>
  </div>

<script>
(function() {
  const vscode = acquireVsCodeApi();
  const messagesEl = document.getElementById('chat-messages');
  const inputEl = document.getElementById('input');
  const sendBtn = document.getElementById('send-btn');

  let currentAssistantEl = null;
  let isStreaming = false;

  // ── Send message ─────────────────────────────────────────────────────
  function sendMessage() {
    const text = inputEl.value.trim();
    if (!text || isStreaming) return;

    // Add user bubble.
    addBubble(text, 'user');
    inputEl.value = '';
    inputEl.style.height = 'auto';

    vscode.postMessage({ type: 'send', text });
    isStreaming = true;
    sendBtn.disabled = true;
    sendBtn.textContent = 'Stop';
    sendBtn.disabled = false;
    sendBtn.onclick = () => {
      vscode.postMessage({ type: 'cancel' });
    };
  }

  sendBtn.addEventListener('click', () => {
    if (isStreaming) {
      vscode.postMessage({ type: 'cancel' });
    } else {
      sendMessage();
    }
  });

  inputEl.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      if (isStreaming) return;
      sendMessage();
    }
  });

  // Auto-resize textarea.
  inputEl.addEventListener('input', () => {
    inputEl.style.height = 'auto';
    inputEl.style.height = Math.min(inputEl.scrollHeight, 120) + 'px';
  });

  // ── Receive messages from extension ──────────────────────────────────
  window.addEventListener('message', (event) => {
    var msg = event.data;
    if (!msg || !msg.type) return;

    if (msg.type === 'process-exit') {
      addBubble('Chat process exited (code ' + msg.code + '). Send a message to restart.', 'status');
      finishStreaming();
    } else {
      handleResponse(msg);
    }
  });

  function handleResponse(resp) {
    switch (resp.type) {
      case 'routing':
        // Show routing badge.
        const badge = document.createElement('div');
        badge.className = 'routing-badge';
        badge.textContent = (resp.tier || '?') + ' \u2192 ' + (resp.model || '?');
        messagesEl.appendChild(badge);
        scrollToBottom();
        break;

      case 'token':
        if (!currentAssistantEl) {
          currentAssistantEl = addBubble('', 'assistant');
        }
        appendToAssistant(resp.text || '');
        scrollToBottom();
        break;

      case 'done':
        if (currentAssistantEl) {
          // Render markdown in the final content.
          const raw = currentAssistantEl.getAttribute('data-raw') || '';
          currentAssistantEl.innerHTML = renderMarkdown(raw);
        }
        currentAssistantEl = null;
        finishStreaming();
        scrollToBottom();
        break;

      case 'error':
        addBubble(resp.error || 'Unknown error', 'error');
        currentAssistantEl = null;
        finishStreaming();
        break;

      case 'status':
        if (resp.text && resp.text !== 'ready') {
          addBubble(resp.text, 'status');
        }
        break;
    }
  }

  // ── DOM helpers ──────────────────────────────────────────────────────
  function addBubble(text, cls) {
    const el = document.createElement('div');
    el.className = 'message ' + cls;
    if (cls === 'assistant') {
      el.setAttribute('data-raw', '');
      el.textContent = '';
    } else if (cls === 'user') {
      el.textContent = text;
    } else {
      el.innerHTML = renderMarkdown(text);
    }
    messagesEl.appendChild(el);
    scrollToBottom();
    return el;
  }

  function appendToAssistant(token) {
    if (!currentAssistantEl) return;
    const raw = (currentAssistantEl.getAttribute('data-raw') || '') + token;
    currentAssistantEl.setAttribute('data-raw', raw);
    currentAssistantEl.textContent = raw;
  }

  function finishStreaming() {
    isStreaming = false;
    sendBtn.textContent = 'Send';
    sendBtn.onclick = null; // reset to default click handler
    inputEl.focus();
  }

  function scrollToBottom() {
    messagesEl.scrollTop = messagesEl.scrollHeight;
  }

  // ── Basic markdown rendering ─────────────────────────────────────────
  var BT = String.fromCharCode(96); // backtick char
  var BT3 = BT + BT + BT;

  function renderMarkdown(text) {
    if (!text) return '';
    var html = escapeHtml(text);

    // Code blocks.
    var codeBlockRe = new RegExp(BT3 + '(\\\\w*)\\\\n([\\\\s\\\\S]*?)' + BT3, 'g');
    html = html.replace(codeBlockRe, function(_, lang, code) {
      return '<pre><code class="lang-' + lang + '">' + code.trim() + '</code></pre>';
    });

    // Inline code.
    var inlineRe = new RegExp(BT + '([^' + BT + ']+)' + BT, 'g');
    html = html.replace(inlineRe, '<code>$1</code>');

    // Bold.
    html = html.replace(/\\*\\*(.+?)\\*\\*/g, '<strong>$1</strong>');

    // Headers.
    html = html.replace(/^### (.+)$/gm, '<strong>$1</strong>');
    html = html.replace(/^## (.+)$/gm, '<strong style="font-size:1.1em">$1</strong>');
    html = html.replace(/^# (.+)$/gm, '<strong style="font-size:1.2em">$1</strong>');

    // Lists.
    html = html.replace(/^[-*] (.+)$/gm, '\\u2022 $1');

    // Newlines.
    html = html.replace(/\\n/g, '<br>');

    return html;
  }

  function escapeHtml(str) {
    return str
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }
})();
</script>
</body>
</html>`;
  }
}
