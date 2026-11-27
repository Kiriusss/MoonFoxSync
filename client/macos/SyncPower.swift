import Foundation
import IOKit.ps

enum SyncPower: String {
    case external, battery, unknown

    static func current() -> SyncPower {
        guard let info = IOPSCopyPowerSourcesInfo()?.takeRetainedValue(),
              let type = IOPSGetProvidingPowerSourceType(info)?.takeUnretainedValue() else { return .unknown }
        switch type as String {
        case kIOPSACPowerValue: return .external
        case kIOPSBatteryPowerValue, kIOPMUPSPowerKey: return .battery
        default: return .unknown
        }
    }
}

enum BatterySyncPolicy: String, CaseIterable {
    case manual, hourly, fourHourly

    var interval: TimeInterval? {
        switch self {
        case .manual: nil
        case .hourly: 3600
        case .fourHourly: 14400
        }
    }

    var label: String {
        switch self {
        case .manual: "仅手动同步"
        case .hourly: "每 1 小时检查"
        case .fourHourly: "每 4 小时检查"
        }
    }
}
