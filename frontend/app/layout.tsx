import type { Metadata } from "next";
import { Inter, JetBrains_Mono } from "next/font/google";
import { Providers } from "@/components/providers";
import "./globals.css";

const inter = Inter({
  subsets: ["latin"],
  variable: "--font-inter",
});

const jetbrainsMono = JetBrains_Mono({
  subsets: ["latin"],
  variable: "--font-mono",
});

export const metadata: Metadata = {
  title: "CryptoBot Dashboard",
  description: "Autonomous Crypto Trading Bot with Local AI",
};

// TODO(CRY-19): replace env-based mode read with a runtime source —
// backend should expose `execution_mode` on /api/v1/risk/status and the
// shell ring should react to that, so flipping to live without a rebuild
// still trips the visual alarm. Env is a build-time fallback only.
const IS_LIVE = process.env.NEXT_PUBLIC_EXECUTION_MODE === "live";

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en" suppressHydrationWarning>
      <body
        className={`${inter.variable} ${jetbrainsMono.variable} font-sans antialiased${
          IS_LIVE ? " ring-2 ring-inset ring-loss" : ""
        }`}
      >
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}
