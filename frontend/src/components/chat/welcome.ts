// Page copy, not model output: a small model improvising a greeting could
// state things about Aleksi that are not true.
export const WELCOME_MESSAGE =
  "Hi, welcome to Aleksi Valta's portfolio. I'm an assistant that answers " +
  'questions about his background, experience and this project, which runs ' +
  "on the stack you're talking to right now. Try asking what he has worked " +
  'on, or how this chat is built. Your messages are sent to an external LLM ' +
  'provider to generate replies, and your IP address is held in memory until ' +
  'midnight UTC for rate limiting.';

// Served from public/, so it reads without JavaScript and links directly.
export const PRIVACY_HREF = '/privacy.html';
