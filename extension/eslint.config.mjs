import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const parser = require("@typescript-eslint/parser");
const plugin = require("@typescript-eslint/eslint-plugin");

export default [
  {
    files: ["src/**/*.ts"],
    languageOptions: {
      parser,
      parserOptions: { sourceType: "module", ecmaVersion: 2022 },
    },
    plugins: { "@typescript-eslint": plugin },
    rules: {
      "no-unused-vars": "off",
      "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_" }],
    },
  },
];
