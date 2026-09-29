import { useId } from "react";

export interface XRayRegion {
  start: number;
  end: number;
  name: string;
}

interface Props {
  size: number;
  regions: XRayRegion[];
  vpStart: number;
  vpEnd: number;
  onSeek: (byte: number) => void;
}

// Cycling palette for region bands (matches the rainbow-CSV family).
const PALETTE = ["#e06c75", "#d19a66", "#e5c07b", "#98c379", "#56b6c2", "#61afef", "#c678dd", "#b48ead"];

export default function XRay({ size, regions, vpStart, vpEnd, onSeek }: Props) {
  const regionDetailsId = useId();
  const fileSize = Number.isFinite(size) ? Math.max(0, Math.floor(size)) : 0;
  const maxByte = Math.max(0, fileSize - 1);
  const clampByte = (byte: number) => Number.isFinite(byte)
    ? Math.max(0, Math.min(maxByte, Math.floor(byte)))
    : 0;
  const clampBoundary = (byte: number) => Number.isFinite(byte)
    ? Math.max(0, Math.min(fileSize, Math.floor(byte)))
    : 0;
  const pct = (byte: number) => fileSize > 0
    ? Math.max(0, Math.min(100, (byte / fileSize) * 100))
    : 0;
  const current = clampByte(vpStart);
  const step = Math.max(1, Math.floor(fileSize / 100));
  const page = Math.max(step, Math.floor(Math.max(0, vpEnd - vpStart)));

  const seekFromPointer = (event: React.MouseEvent<HTMLButtonElement>) => {
    // Native button keyboard activation does not have a useful pointer Y.
    if (event.detail === 0 || fileSize === 0) return;
    const target = event.target instanceof HTMLElement
      ? event.target.closest<HTMLElement>("[data-region-start]")
      : null;
    if (target?.dataset.regionStart != null) {
      onSeek(clampByte(Number(target.dataset.regionStart)));
      return;
    }
    const bounds = event.currentTarget.getBoundingClientRect();
    if (bounds.height <= 0) return;
    const y = (event.clientY - bounds.top) / bounds.height;
    onSeek(clampByte(y * fileSize));
  };

  const seekFromKeyboard = (event: React.KeyboardEvent<HTMLButtonElement>) => {
    let next: number | undefined;
    switch (event.key) {
      case "ArrowUp":
      case "ArrowLeft":
        next = current - step;
        break;
      case "ArrowDown":
      case "ArrowRight":
        next = current + step;
        break;
      case "PageUp":
        next = current - page;
        break;
      case "PageDown":
        next = current + page;
        break;
      case "Home":
        next = 0;
        break;
      case "End":
        next = maxByte;
        break;
      default:
        return;
    }
    event.preventDefault();
    if (fileSize > 0) onSeek(clampByte(next));
  };

  return (
    <>
      <button
        type="button"
        className="q-xray"
        role="slider"
        aria-label="File map position"
        aria-describedby={regionDetailsId}
        aria-orientation="vertical"
        aria-valuemin={0}
        aria-valuemax={maxByte}
        aria-valuenow={current}
        aria-valuetext={fileSize === 0
          ? "Empty file"
          : `Byte ${current} of ${maxByte}; visible byte range ${clampBoundary(vpStart)} up to but not including ${clampBoundary(vpEnd)}; ${regions.length} analyzed regions`}
        disabled={fileSize === 0}
        title="File map — click or use arrow keys to jump"
        onClick={seekFromPointer}
        onKeyDown={seekFromKeyboard}
      >
        {regions.map((region, index) => (
          <span
            key={`${region.start}:${region.end}:${region.name}`}
            className="q-xray-band"
            title={region.name}
            aria-hidden="true"
            data-region-start={clampByte(region.start)}
            style={{
              top: pct(region.start) + "%",
              height: Math.max(0.25, pct(region.end) - pct(region.start)) + "%",
              background: PALETTE[index % PALETTE.length],
            }}
          />
        ))}
        <span
          className="q-xray-vp"
          aria-hidden="true"
          style={{ top: pct(vpStart) + "%", height: Math.max(0.8, pct(vpEnd) - pct(vpStart)) + "%" }}
        />
      </button>
      <div id={regionDetailsId} className="q-sr-only">
        {regions.length === 0 ? (
          "No analyzed regions."
        ) : (
          <>
            <span>Analyzed file regions:</span>
            <ul>
              {regions.map((region, index) => (
                <li key={`${region.start}:${region.end}:${region.name}:accessible`}>
                  {region.name.trim() || `Region ${index + 1}`}: byte range {clampBoundary(region.start)} up to but not including {clampBoundary(region.end)}
                </li>
              ))}
            </ul>
          </>
        )}
      </div>
    </>
  );
}
