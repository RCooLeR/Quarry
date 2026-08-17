export interface ActiveRequestToken {
  readonly channel: string;
  readonly fileId: string;
  readonly generation: number;
  readonly activeEpoch: number;
}

export interface FileRequestToken {
  readonly channel: string;
  readonly fileId: string;
  readonly generation: number;
}

export interface ActiveContext {
  readonly fileId: string;
  readonly activeEpoch: number;
}

/**
 * Owns the monotonically increasing generations used by App-level async work.
 * Active requests are invalidated both by a newer request on the same channel
 * and by any editor-file transition. File requests are independently ordered
 * per file so a slow staging/session response cannot replace a newer one.
 */
export class RequestGenerationGate {
  private nextGeneration = 0;
  private activeEpoch = 0;
  private readonly latest = new Map<string, number>();

  beginActive(channel: string, fileId: string): ActiveRequestToken {
    const generation = this.bump(channel);
    return { channel, fileId, generation, activeEpoch: this.activeEpoch };
  }

  beginFile(channel: string, fileId: string): FileRequestToken {
    const key = this.fileChannel(channel, fileId);
    const generation = this.bump(key);
    return { channel: key, fileId, generation };
  }

  captureActive(fileId: string): ActiveContext {
    return { fileId, activeEpoch: this.activeEpoch };
  }

  invalidateActive(): void {
    this.activeEpoch++;
  }

  invalidateChannel(channel: string): void {
    this.bump(channel);
  }

  forgetFile(channel: string, fileId: string): void {
    this.latest.delete(this.fileChannel(channel, fileId));
  }

  isLatest(token: ActiveRequestToken | FileRequestToken): boolean {
    return this.latest.get(token.channel) === token.generation;
  }

  isActiveContext(
    context: ActiveContext,
    activeFileId: string | null,
    editorFileId: string | null,
  ): boolean {
    return context.activeEpoch === this.activeEpoch
      && context.fileId === activeFileId
      && context.fileId === editorFileId;
  }

  isActive(
    token: ActiveRequestToken,
    activeFileId: string | null,
    editorFileId: string | null,
  ): boolean {
    return this.isLatest(token)
      && this.isActiveContext(token, activeFileId, editorFileId);
  }

  private bump(channel: string): number {
    const generation = ++this.nextGeneration;
    this.latest.set(channel, generation);
    return generation;
  }

  private fileChannel(channel: string, fileId: string): string {
    return `${channel}\u0000${fileId}`;
  }
}
