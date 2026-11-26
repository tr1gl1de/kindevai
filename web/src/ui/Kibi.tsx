/** Kibi's moods used in the UI: idle for an empty list, success when an empty list means all is done. */
export type KibiMood = "idle" | "success";

/**
 * Kibi, the Kindevai mascot (Brand System v1.0, assets/mascot/kibi-*.svg), drawn inline like BrandLogo: the Charcoal
 * strokes take the text colour so Kibi stays visible on dark themes; Honey and Sage stay fixed. Decorative only —
 * the empty state's heading says what it means. For benign empty states only, never on errors (brandbook §05).
 */
export function Kibi({ mood }: { mood: KibiMood }) {
  return (
    <svg className="kibi" data-mood={mood} aria-hidden="true" focusable="false" viewBox="0 0 240 210" fill="none" xmlns="http://www.w3.org/2000/svg">
      <g strokeLinejoin="round" strokeLinecap="round">
        <path d="M72 127 83 137 83 162 61 175 40 162 40 138Z M168 127 157 137 157 162 179 175 200 162 200 138Z" fill="#899680" />
        <g stroke="currentColor" strokeWidth="11">
          <path d="M111 66 Q110 37 88 23 M129 66 Q130 37 152 23" />
          <circle cx="86" cy="22" r="6" fill="currentColor" />
          <circle cx="154" cy="22" r="6" fill="currentColor" />
          <path d="M80 67 Q72 63 63 67 L32 85 Q24 90 24 100 V121 Q24 132 33 137 L63 154 Q72 159 81 154 L104 141 V81Z" />
          <path d="M160 67 Q168 63 177 67 L208 85 Q216 90 216 100 V121 Q216 132 207 137 L177 154 Q168 159 159 154 L136 141 V81Z" />
          <path d="M82 133 120 113 158 133 V166 Q158 175 150 180 L128 193 Q120 198 112 193 L90 180 Q82 175 82 166Z" fill="#FFB928" />
          <path d="M112 67 Q120 62 128 67 L155 83 Q163 88 163 98 V120 Q163 130 155 135 L128 151 Q120 156 112 151 L85 135 Q77 130 77 120 V98 Q77 88 85 83Z" fill="#FFB928" />
        </g>
        <path d="M96 111 Q102 98 108 111 M132 111 Q138 98 144 111" stroke="currentColor" strokeWidth="6.5" />
      </g>
      {mood === "success" && (
        <>
          <circle cx="201" cy="169" r="24" fill="#28734D" />
          <path d="m189 169 8 8 15 -17" stroke="#FFFFFF" strokeWidth="5" strokeLinecap="round" />
        </>
      )}
    </svg>
  );
}
