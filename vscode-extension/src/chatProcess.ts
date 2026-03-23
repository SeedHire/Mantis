import * as vscode from 'vscode';
import { ChildProcess, spawn } from 'child_process';
import { EventEmitter } from 'events';
import * as readline from 'readline';

/** Inbound JSON response from `mantis chat --json`. */
export interface ChatResponse {
  id: number;
  type: 'token' | 'done' | 'file_write' | 'error' | 'status' | 'routing';
  text?: string;
  model?: string;
  tier?: string;
  path?: string;
  preview?: string;
  tokens?: number;
  error?: string;
}

/**
 * Manages a `mantis chat --json` child process.
 * Communicates via JSON Lines on stdin/stdout.
 */
export class ChatProcess extends EventEmitter {
  private proc: ChildProcess | null = null;
  private nextId = 1;
  private cwd: string;
  private binaryPath: string;

  constructor(cwd: string) {
    super();
    this.cwd = cwd;
    const config = vscode.workspace.getConfiguration('mantis');
    this.binaryPath = config.get<string>('binaryPath', 'mantis');
  }

  /** Start the chat subprocess. */
  start(): void {
    if (this.proc) {
      return;
    }

    this.proc = spawn(this.binaryPath, ['chat', '--json', '--offline'], {
      cwd: this.cwd,
      stdio: ['pipe', 'pipe', 'pipe'],
      env: { ...process.env },
    });

    if (!this.proc.stdout || !this.proc.stdin) {
      this.emit('error', new Error('Failed to open stdio pipes'));
      return;
    }

    const rl = readline.createInterface({ input: this.proc.stdout });
    rl.on('line', (line: string) => {
      if (!line.trim()) {
        return;
      }
      try {
        const resp: ChatResponse = JSON.parse(line);
        this.emit('response', resp);
      } catch {
        // Ignore non-JSON lines (e.g. startup logs on stderr).
      }
    });

    this.proc.stderr?.on('data', (data: Buffer) => {
      const msg = data.toString().trim();
      if (msg) {
        this.emit('stderr', msg);
      }
    });

    this.proc.on('exit', (code: number | null) => {
      this.proc = null;
      this.emit('exit', code);
    });

    this.proc.on('error', (err: Error) => {
      this.proc = null;
      this.emit('error', err);
    });
  }

  /** Send a chat message. Returns the request ID. */
  sendChat(message: string): number {
    const id = this.nextId++;
    this.send({ id, method: 'chat', params: { message } });
    return id;
  }

  /** Send a slash command. Returns the request ID. */
  sendCommand(name: string, args?: string): number {
    const id = this.nextId++;
    this.send({ id, method: 'command', params: { name, args: args || '' } });
    return id;
  }

  /** Cancel the current in-progress request. */
  cancel(): void {
    this.send({ id: 0, method: 'cancel', params: {} });
  }

  /** Stop the process. */
  stop(): void {
    if (this.proc) {
      this.proc.kill();
      this.proc = null;
    }
  }

  /** Whether the process is running. */
  get isRunning(): boolean {
    return this.proc !== null;
  }

  private send(msg: object): void {
    if (!this.proc?.stdin?.writable) {
      this.emit('error', new Error('Chat process not running'));
      return;
    }
    this.proc.stdin.write(JSON.stringify(msg) + '\n');
  }
}
