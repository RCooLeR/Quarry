export interface ToolsRequestToken {
  readonly channel: string;
  readonly requestEpoch: number;
  readonly mountEpoch: number;
  readonly contextEpoch: number;
  readonly fileId: string;
  readonly detected: string;
  readonly csvConfigurationEpoch?: number;
  readonly sourceGeneration?: number;
}

interface BeginRequestOptions {
  csvConfiguration?: boolean;
  sourceGeneration?: number;
}

/**
 * Orders async work owned by the Tools panel. Tokens are tied to the mounted
 * component instance and its current file/type context. CSV tokens may also be
 * tied to the active dialect/schema configuration and source generation.
 */
export class ToolsRequestEpochGate {
  private mounted = true;
  private mountEpoch = 1;
  private contextEpoch = 1;
  private csvConfigurationEpoch = 0;
  private nextRequestEpoch = 0;
  private fileId: string;
  private detected: string;
  private readonly latest = new Map<string, number>();

  constructor(fileId: string, detected: string) {
    this.fileId = fileId;
    this.detected = detected;
  }

  mount(): void {
    if (this.mounted) return;
    this.mounted = true;
    this.mountEpoch++;
  }

  unmount(): void {
    if (!this.mounted) return;
    this.mounted = false;
    this.mountEpoch++;
    this.latest.clear();
  }

  syncContext(fileId: string, detected: string): boolean {
    if (fileId === this.fileId && detected === this.detected) return false;
    this.fileId = fileId;
    this.detected = detected;
    this.contextEpoch++;
    this.csvConfigurationEpoch++;
    this.latest.clear();
    return true;
  }

  advanceCsvConfiguration(): number {
    this.csvConfigurationEpoch++;
    return this.csvConfigurationEpoch;
  }

  invalidateChannel(channel: string): void {
    this.latest.set(channel, ++this.nextRequestEpoch);
  }

  begin(channel: string, options: BeginRequestOptions = {}): ToolsRequestToken {
    const requestEpoch = ++this.nextRequestEpoch;
    this.latest.set(channel, requestEpoch);
    return {
      channel,
      requestEpoch,
      mountEpoch: this.mountEpoch,
      contextEpoch: this.contextEpoch,
      fileId: this.fileId,
      detected: this.detected,
      ...(options.csvConfiguration ? { csvConfigurationEpoch: this.csvConfigurationEpoch } : {}),
      ...(options.sourceGeneration == null ? {} : { sourceGeneration: options.sourceGeneration }),
    };
  }

  isCurrent(token: ToolsRequestToken, sourceGeneration?: number | null): boolean {
    return this.mounted
      && token.mountEpoch === this.mountEpoch
      && token.contextEpoch === this.contextEpoch
      && token.fileId === this.fileId
      && token.detected === this.detected
      && this.latest.get(token.channel) === token.requestEpoch
      && (token.csvConfigurationEpoch == null || token.csvConfigurationEpoch === this.csvConfigurationEpoch)
      && (token.sourceGeneration == null || token.sourceGeneration === sourceGeneration);
  }
}
