// @vitest-environment node
// KIN-121: the Telegram side of the Kindevai brand. BotFather settings live outside the repo, so the repo ships the
// assets at the sizes Telegram takes, the bot texts within Telegram's limits, and the README steps that point to them.
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../../../", import.meta.url));
const telegram = `${root}docs/brand/telegram/`;

// Width, height and colour type from a PNG's IHDR chunk.
function png(file: string): {
  width: number;
  height: number;
  colorType: number;
} {
  const b = readFileSync(file);
  expect(b.subarray(0, 8).toString("hex")).toBe("89504e470d0a1a0a");
  expect(b.subarray(12, 16).toString("ascii")).toBe("IHDR");
  return {
    width: b.readUInt32BE(16),
    height: b.readUInt32BE(20),
    colorType: b[25]!,
  };
}

const RGB = 2; // truecolour without alpha: no transparent corners for Telegram or iOS to fill with black

describe("Telegram assets", () => {
  it.each([
    ["docs/brand/telegram/bot-avatar-640.png", 640, 640], // bot profile photo (/setuserpic), cropped to a circle
    ["docs/brand/telegram/miniapp-photo-640x360.png", 640, 360], // Mini App photo asked by /newapp and /editapp
    ["web/public/apple-touch-icon.png", 180, 180], // home-screen icon when the web UI is added to a phone
  ])("%s is an opaque %ix%i PNG", (file, width, height) => {
    expect(png(`${root}${file}`)).toEqual({ width, height, colorType: RGB });
  });

  it("ships the SVG sources the PNGs are rendered from", () => {
    for (const f of ["bot-avatar.svg", "miniapp-photo.svg"]) {
      const svg = readFileSync(`${telegram}${f}`, "utf8");
      expect(svg).toContain('fill="#F9F6EC"'); // Cream background
      expect(svg).toContain('fill="#FFB928"'); // Honey body of the mark
    }
  });

  it("links the apple-touch icon from the web UI", () => {
    expect(readFileSync(`${root}web/index.html`, "utf8")).toContain(
      '<link rel="apple-touch-icon" href="/apple-touch-icon.png" />',
    );
  });
});

describe("bot profile texts", () => {
  type Profile = {
    name: string;
    short_description: string;
    description: string;
  };
  const profile = JSON.parse(
    readFileSync(`${telegram}bot-profile.json`, "utf8"),
  ) as Record<string, Profile>;

  it("comes in English and Russian", () => {
    expect(Object.keys(profile).sort()).toEqual(["en", "ru"]);
  });

  it.each(["en", "ru"])(
    "%s: names the bot Kindevai and fits Telegram's limits",
    (lang) => {
      const p = profile[lang]!;
      expect(p.name).toBe("Kindevai");
      expect([...p.name].length).toBeLessThanOrEqual(64); // setMyName
      expect([...p.short_description].length).toBeLessThanOrEqual(120); // setMyShortDescription / "About"
      expect([...p.description].length).toBeLessThanOrEqual(512); // setMyDescription / "What can this bot do?"
      expect(p.short_description).toContain("Kindevai");
      expect(p.description).toContain("Kindevai");
    },
  );

  it("writes the slogan in the bot description", () => {
    expect(profile.en!.description).toContain(
      "Collective intelligence. Individual care.",
    );
    expect(profile.ru!.description).toContain(
      "Коллективный интеллект. Индивидуальная забота.",
    );
  });
});

describe("README", () => {
  it.each(["README.md", "README.ru.md"])(
    "%s tells the owner where the Telegram assets are",
    (f) => {
      const md = readFileSync(`${root}${f}`, "utf8");
      for (const s of [
        "docs/brand/telegram/bot-avatar-640.png",
        "docs/brand/telegram/miniapp-photo-640x360.png",
        "docs/brand/telegram/bot-profile.json",
        "/setuserpic",
        "/setname",
        "/setabouttext",
        "/setdescription",
        "language_code=ru",
        // the Bot API method each bot-profile.json field goes to, as the README loop builds them
        "Name:name ShortDescription:short_description Description:description",
      ]) {
        expect(md).toContain(s);
      }
      expect(existsSync(`${telegram}bot-profile.json`)).toBe(true);
    },
  );
});
