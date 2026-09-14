PROGRAM Plant
(* The logic is deliberately tiny — this example is about the WORKFLOW:
   devices.yaml → import → manifest + tag file → serve/run. It averages the
   analyser, raises a concentration alarm, and passes the operator's
   setpoints through to the writable Modbus bindings. *)
VAR_EXTERNAL
    (* driver inputs — tags/modbus.yaml *)
    GAS_CH1  : REAL;
    GAS_CH2  : REAL;
    GAS_CH3  : REAL;
    GAS_CH4  : REAL;
    TC_A_Tpv : REAL;
    (* driver outputs — writable bindings *)
    TC_A_Tsp    : REAL;
    TC_B_Tsp    : REAL;
    FAN_Ssp     : REAL;
    FAN_Control : REAL;
    (* this project's own tags — nautilus.yaml *)
    CFG_HeatersOn : BOOL;
    TspCmd  : REAL;
    FanCmd  : REAL;
    AvgPPM  : REAL;
    TErr    : REAL;
    HighAlm : BOOL;
END_VAR

AvgPPM := (GAS_CH1 + GAS_CH2 + GAS_CH3 + GAS_CH4) / 4.0;
HighAlm := AvgPPM > 250.0;

(* Both zones track the one operator setpoint; the driver inverts the
   x10 scaling on the way to the gateway. *)
TC_A_Tsp := TspCmd;
TC_B_Tsp := TspCmd;

(* CFG_HeatersOn is also the manifest's `enable:` tag — false parks the
   TC sources, so the zone error is meaningless while they're off. *)
IF CFG_HeatersOn THEN
    TErr := TspCmd - TC_A_Tpv;
ELSE
    TErr := 0.0;
END_IF;

FAN_Ssp := FanCmd;
IF HighAlm THEN
    FAN_Control := 1.0;
ELSE
    FAN_Control := 0.0;
END_IF;

END_PROGRAM
