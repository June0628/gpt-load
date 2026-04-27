/**
 * Type shim for @vicons/ionicons5
 *
 * The package's .d.ts files use ESM export syntax but don't declare "type": "module"
 * in package.json, which causes TS2305 errors under verbatimModuleSyntax: true.
 * This declaration file provides proper type definitions for the icons we use.
 */
declare module "@vicons/ionicons5" {
  import type { DefineComponent } from "vue";

  type IconComponent = DefineComponent<object, object, unknown>;

  export const Add: IconComponent;
  export const AddCircleOutline: IconComponent;
  export const AlertCircleOutline: IconComponent;
  export const BugOutline: IconComponent;
  export const ChatbubbleOutline: IconComponent;
  export const CheckmarkCircle: IconComponent;
  export const CheckmarkCircleOutline: IconComponent;
  export const CheckmarkDoneOutline: IconComponent;
  export const ChevronBack: IconComponent;
  export const ChevronDown: IconComponent;
  export const ChevronForward: IconComponent;
  export const ChevronUp: IconComponent;
  export const Close: IconComponent;
  export const CloseCircleOutline: IconComponent;
  export const CloseOutline: IconComponent;
  export const CloudUploadOutline: IconComponent;
  export const Contrast: IconComponent;
  export const Copy: IconComponent;
  export const CopyOutline: IconComponent;
  export const CreateOutline: IconComponent;
  export const DocumentTextOutline: IconComponent;
  export const DownloadOutline: IconComponent;
  export const EyeOffOutline: IconComponent;
  export const EyeOutline: IconComponent;
  export const HelpCircle: IconComponent;
  export const HelpCircleOutline: IconComponent;
  export const InformationCircleOutline: IconComponent;
  export const Key: IconComponent;
  export const Language: IconComponent;
  export const LinkOutline: IconComponent;
  export const LockClosedSharp: IconComponent;
  export const LogOutOutline: IconComponent;
  export const LogoGithub: IconComponent;
  export const Moon: IconComponent;
  export const Pencil: IconComponent;
  export const PeopleOutline: IconComponent;
  export const ReloadOutline: IconComponent;
  export const Remove: IconComponent;
  export const RemoveCircleOutline: IconComponent;
  export const Save: IconComponent;
  export const Search: IconComponent;
  export const SettingsOutline: IconComponent;
  export const Sunny: IconComponent;
  export const TimeOutline: IconComponent;
  export const Trash: IconComponent;
  export const WarningOutline: IconComponent;
}
