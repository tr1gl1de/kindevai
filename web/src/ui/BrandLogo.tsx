import { useTranslation } from "react-i18next";

/**
 * The Kindevai lockup (Brand System v1.0, docs/brand/lockup-*.svg) drawn inline: the Charcoal strokes of the light
 * lockup take the text colour, so it matches the dark lockup's Cream on dark themes and Telegram's text colour in
 * the Mini App. Honey and Sage stay fixed. The wordmark is artwork, never set in the interface font.
 */
export function BrandLogo() {
  const { t } = useTranslation();
  return (
    <svg className="brand-logo" role="img" aria-label={t("app.title")} viewBox="0 0 655 140" fill="none" xmlns="http://www.w3.org/2000/svg">
      <g transform="translate(0 0) scale(.65)">
      <g strokeLinejoin="round" strokeLinecap="round">
      <path d="M72 127 83 137 83 162 61 175 40 162 40 138Z M168 127 157 137 157 162 179 175 200 162 200 138Z" fill="#899680"/>
      <g stroke="currentColor" strokeWidth="11">
      <path d="M111 66 Q110 37 88 23 M129 66 Q130 37 152 23"/>
      <circle cx="86" cy="22" r="6" fill="currentColor"/>
      <circle cx="154" cy="22" r="6" fill="currentColor"/>
      <path d="M80 67 Q72 63 63 67 L32 85 Q24 90 24 100 V121 Q24 132 33 137 L63 154 Q72 159 81 154 L104 141 V81Z" fill="none"/>
      <path d="M160 67 Q168 63 177 67 L208 85 Q216 90 216 100 V121 Q216 132 207 137 L177 154 Q168 159 159 154 L136 141 V81Z" fill="none"/>
      <path d="M82 133 120 113 158 133 V166 Q158 175 150 180 L128 193 Q120 198 112 193 L90 180 Q82 175 82 166Z" fill="#FFB928"/>
      <path d="M112 67 Q120 62 128 67 L155 83 Q163 88 163 98 V120 Q163 130 155 135 L128 151 Q120 156 112 151 L85 135 Q77 130 77 120 V98 Q77 88 85 83Z" fill="#FFB928"/>
      </g>
      <path d="M96 111 Q102 98 108 111 M132 111 Q138 98 144 111" stroke="currentColor" strokeWidth="6.5"/>
      </g>
      </g>
      <g transform="translate(165 17)">
      <g transform="translate(10 15)">
      <path d="M0 0 V76 M0 48 L38 19 M12 39 L40 76" stroke="currentColor" strokeWidth="15" strokeLinecap="round" strokeLinejoin="round"/>
      </g>
      <g transform="translate(69 15)">
      <path d="M0 21 V76" stroke="currentColor" strokeWidth="15" strokeLinecap="round" strokeLinejoin="round"/>
      <circle cy="0" r="8" fill="currentColor"/>
      </g>
      <g transform="translate(94 15)">
      <path d="M0 76 V45 Q0 20 24 20 Q47 20 47 45 V76" stroke="currentColor" strokeWidth="15" strokeLinecap="round" strokeLinejoin="round"/>
      </g>
      <g transform="translate(156 15)">
      <path d="M48 0 V76 M48 48 A24 28 0 1 0 0 48 A24 28 0 0 0 48 48" stroke="currentColor" strokeWidth="15" strokeLinecap="round" strokeLinejoin="round"/>
      </g>
      <g transform="translate(220 15)">
      <path d="M0 54 L48 39 Q43 19 24 20 A24 28 0 1 0 44 69" stroke="currentColor" strokeWidth="15" strokeLinecap="round" strokeLinejoin="round"/>
      </g>
      <g transform="translate(283 15)">
      <path d="M0 22 L24 76 48 22" stroke="currentColor" strokeWidth="15" strokeLinecap="round" strokeLinejoin="round"/>
      </g>
      <g transform="translate(346 15)">
      <path d="M48 76 V47 A24 27 0 1 0 0 47 A24 27 0 0 0 48 47" stroke="currentColor" strokeWidth="15" strokeLinecap="round" strokeLinejoin="round"/>
      </g>
      <g transform="translate(415 15)">
      <path d="M0 21 V76" stroke="currentColor" strokeWidth="15" strokeLinecap="round" strokeLinejoin="round"/>
      <path d="M0 -10 9 -5 9 5 0 10 -9 5 -9 -5Z" fill="#FFB928"/>
      </g>
      </g>
    </svg>
  );
}
