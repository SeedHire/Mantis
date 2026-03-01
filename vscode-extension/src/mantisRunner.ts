import * as cp from 'child_process';
import * as vscode from 'vscode';

export interface RunResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

/** Spawns the mantis CLI and returns the result. */
export async function runMantis(
  args: string[],
  cwd?: string
): Promise<RunResult> {
  const config = vscode.workspace.getConfiguration('mantis');
  const binary: string = config.get('binaryPath') ?? 'mantis';
  const workspaceRoot =
    cwd ??
    vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ??
    process.cwd();

  return new Promise((resolve) => {
    let stdout = '';
    let stderr = '';

    const child = cp.spawn(binary, args, {
      cwd: workspaceRoot,
      env: { ...process.env },
    });

    child.stdout.on('data', (d: Buffer) => (stdout += d.toString()));
    child.stderr.on('data', (d: Buffer) => (stderr += d.toString()));

    child.on('close', (code) => {
      resolve({ stdout, stderr, exitCode: code ?? 0 });
    });

    child.on('error', (err) => {
      resolve({
        stdout: '',
        stderr: `Failed to start mantis binary "${binary}": ${err.message}`,
        exitCode: 1,
      });
    });
  });
}

/** Returns the workspace root path. */
export function getWorkspaceRoot(): string | undefined {
  return vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
}

/** Parses `mantis lint` tabular output into violation objects. */
export interface LintViolation {
  severity: string;
  location: string;
  message: string;
}

export function parseLintOutput(output: string): LintViolation[] {
  const violations: LintViolation[] = [];
  const lines = output.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trim();
    const match = line.match(/^(ERROR|WARNING)\s+(.+)$/i);
    if (match) {
      const message = lines[i + 1]?.trim() ?? '';
      violations.push({
        severity: match[1].toLowerCase(),
        location: match[2].trim(),
        message,
      });
      i++;
    }
  }
  return violations;
}

/** Parses `mantis find --format json` output into file paths. */
export function parseFindOutput(output: string): string[] {
  try {
    const parsed = JSON.parse(output);
    if (Array.isArray(parsed)) {
      return parsed as string[];
    }
  } catch {
    // Not JSON — fall through
  }
  return [];
}
