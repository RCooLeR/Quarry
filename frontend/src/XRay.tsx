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
  const pct = (b: number) => (size > 0 ? (b / size) * 100 : 0);

  const seekFromEvent = (e: React.MouseEvent<HTMLDivElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    const y = (e.clientY - r.top) / r.height;
    onSeek(Math.max(0, Math.min(size - 1, Math.floor(y * size))));
  };

  return (
    <div className="q-xray" title="File map — click to jump" onClick={seekFromEvent}>
      {regions.map((rg, i) => (
        <div
          key={i}
          className="q-xray-band"
          title={rg.name}
          style={{
            top: pct(rg.start) + "%",
            height: Math.max(0.25, ((rg.end - rg.start) / size) * 100) + "%",
            background: PALETTE[i % PALETTE.length],
          }}
          onClick={(e) => { e.stopPropagation(); onSeek(rg.start); }}
        />
      ))}
      <div
        className="q-xray-vp"
        style={{ top: pct(vpStart) + "%", height: Math.max(0.8, ((vpEnd - vpStart) / size) * 100) + "%" }}
      />
    </div>
  );
}
